package nebius

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/nebius/gosdk"
	nebiuscommonv1 "github.com/nebius/gosdk/proto/nebius/common/v1"
	nebiuscomputev1 "github.com/nebius/gosdk/proto/nebius/compute/v1"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance"
	labelspkg "github.com/Azure/karpenter-provider-azure/pkg/providers/labels"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/cloudproviders"
	stretchoptions "github.com/Azure/karpenter-provider-azure/pkg/stretch/options"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/userdata"
)

const instanceNamePrefix = "stretch-nebius-"

type vmInstanceConfig struct {
	ProjectID     string
	InstanceName  string
	Platform      string // e.g., "cpu-d3", "cpu-e2"
	Preset        string // e.g., "4vcpu-16gb"
	SubnetID      string
	ImageFamily   string // e.g., "ubuntu24.04-driverless"
	DiskSizeGB    int32
	CloudInitData string // cloud-init user data
}

func newVMInstanceConfig(
	ctx context.Context,
	nodeClass *v1beta1.StretchNebiusNodeClass,
	nodeClaim *v1.NodeClaim,
	platformPreset *platformPreset,
	aksClusterRestConfig *rest.Config,
) (*vmInstanceConfig, error) {
	karpOpts := options.FromContext(ctx) // FIXME: this pattern is not great
	userData, err := userdata.UserData(karpOpts, aksClusterRestConfig, []string{
		cloudproviders.NodeClaimLabelKeyValue(nodeClaim),
	})
	if err != nil {
		return nil, fmt.Errorf("generating user data: %w", err)
	}
	userDataEncoded, err := userData.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshaling user data: %w", err)
	}

	rv := &vmInstanceConfig{
		ProjectID: stretchoptions.MustGetNebiusProjectID(ctx), // FIXME: maybe resolve from node class?
		// FIXME: confirm naming pattern in nebius side
		InstanceName: fmt.Sprintf("%s%s", instanceNamePrefix, nodeClaim.Name),
		Platform:     platformPreset.platform.GetMetadata().GetName(),
		Preset:       platformPreset.preset.GetName(),
		SubnetID:     nodeClass.Spec.SubnetID,
		// FIXME: resolve default image from nebius instead
		ImageFamily:   lo.FromPtrOr(nodeClass.Spec.OSDiskImageFamily, "ubuntu24.04-driverless"),
		DiskSizeGB:    lo.FromPtrOr(nodeClass.Spec.OSDiskSizeGB, int32(100)),
		CloudInitData: string(userDataEncoded),
	}

	// TODO: validate settings values

	return rv, nil
}

type vmInstance struct {
	config *vmInstanceConfig
	sdk    *gosdk.SDK
}

func newVMInstance(
	instanceConfig *vmInstanceConfig,
	sdk *gosdk.SDK,
) *vmInstance {
	return &vmInstance{
		sdk:    sdk,
		config: instanceConfig,
	}
}

func (i *vmInstance) resolveBootDiskMetadata() *nebiuscommonv1.ResourceMetadata {
	return &nebiuscommonv1.ResourceMetadata{
		ParentId: i.config.ProjectID,
		Name:     fmt.Sprintf("%s-boot-disk", i.config.InstanceName),
	}
}

func (i *vmInstance) resolveInstanceMetadata() *nebiuscommonv1.ResourceMetadata {
	return &nebiuscommonv1.ResourceMetadata{
		ParentId: i.config.ProjectID,
		Name:     i.config.InstanceName,
	}
}

func (i *vmInstance) getBootDisk(
	ctx context.Context,
	metadata *nebiuscommonv1.ResourceMetadata,
) (*nebiuscomputev1.Disk, error) {
	diskService := i.sdk.Services().Compute().V1().Disk()
	return diskService.GetByName(ctx, &nebiuscommonv1.GetByNameRequest{
		ParentId: metadata.ParentId,
		Name:     metadata.Name,
	})
}

func (i *vmInstance) provisionBootDisk(ctx context.Context) (string, error) {
	diskMetadata := i.resolveBootDiskMetadata()
	disk, err := i.getBootDisk(ctx, diskMetadata)
	switch {
	case err == nil:
		// disk already exists
		return disk.Metadata.Id, nil
	case isNotFound(err):
		// not found, proceed to create
	default:
		return "", fmt.Errorf("checking for existing boot disk: %w", err)
	}

	diskService := i.sdk.Services().Compute().V1().Disk()
	createDiskReq := &nebiuscomputev1.CreateDiskRequest{
		Metadata: diskMetadata,
		Spec: &nebiuscomputev1.DiskSpec{
			Size: &nebiuscomputev1.DiskSpec_SizeGibibytes{
				SizeGibibytes: int64(i.config.DiskSizeGB),
			},
			Type: nebiuscomputev1.DiskSpec_NETWORK_SSD,
			Source: &nebiuscomputev1.DiskSpec_SourceImageFamily{
				SourceImageFamily: &nebiuscomputev1.SourceImageFamily{
					ImageFamily: i.config.ImageFamily,
				},
			},
		},
	}
	op, err := diskService.Create(ctx, createDiskReq)
	if err != nil {
		if isAlreadyExists(err) {
			// guard against concurrent creations
			disk, getErr := i.getBootDisk(ctx, diskMetadata)
			if getErr == nil {
				return disk.Metadata.Id, nil
			}
			return "", getErr
		}
		return "", err
	}
	op, pollErr := op.Wait(ctx)
	if pollErr != nil {
		return "", fmt.Errorf("waiting for boot disk creation operation: %w", pollErr)
	}

	return op.ResourceID(), nil
}

func (i *vmInstance) getInstance(
	ctx context.Context,
	metadata *nebiuscommonv1.ResourceMetadata,
) (*nebiuscomputev1.Instance, error) {
	instanceService := i.sdk.Services().Compute().V1().Instance()
	return instanceService.GetByName(ctx, &nebiuscommonv1.GetByNameRequest{
		ParentId: metadata.ParentId,
		Name:     metadata.Name,
	})
}

func (i *vmInstance) provisionInstance(ctx context.Context, bootDiskID string) error {
	instanceMetadata := i.resolveInstanceMetadata()
	_, err := i.getInstance(ctx, instanceMetadata)
	switch {
	case err == nil:
		// instance already exists
		return nil
	case isNotFound(err):
		// not found, proceed to create
	default:
		return fmt.Errorf("checking for existing instance: %w", err)
	}

	instanceService := i.sdk.Services().Compute().V1().Instance()

	spec := &nebiuscomputev1.InstanceSpec{
		Resources: &nebiuscomputev1.ResourcesSpec{
			Platform: i.config.Platform,
			Size: &nebiuscomputev1.ResourcesSpec_Preset{
				Preset: i.config.Preset,
			},
		},
		BootDisk: &nebiuscomputev1.AttachedDiskSpec{
			AttachMode: nebiuscomputev1.AttachedDiskSpec_READ_WRITE,
			Type: &nebiuscomputev1.AttachedDiskSpec_ExistingDisk{
				ExistingDisk: &nebiuscomputev1.ExistingDisk{
					Id: bootDiskID,
				},
			},
		},
		NetworkInterfaces: []*nebiuscomputev1.NetworkInterfaceSpec{
			{
				SubnetId:  i.config.SubnetID,
				Name:      "eth0",
				IpAddress: &nebiuscomputev1.IPAddress{
					// Auto-allocate private IP
				},
				PublicIpAddress: &nebiuscomputev1.PublicIPAddress{
					// Request ephemeral public IP
				},
			},
		},
		CloudInitUserData: i.config.CloudInitData,
	}

	op, err := instanceService.Create(ctx, &nebiuscomputev1.CreateInstanceRequest{
		Metadata: instanceMetadata,
		Spec:     spec,
	})
	if err != nil {
		if isAlreadyExists(err) {
			// guard against concurrent creations
			_, getErr := i.getInstance(ctx, instanceMetadata)
			return getErr
		}
		return err
	}
	op, pollErr := op.Wait(ctx)
	if pollErr != nil {
		return fmt.Errorf("waiting for instance creation operation: %w", pollErr)
	}

	return nil
}

func (i *vmInstance) Provision(ctx context.Context) error {
	logger := log.FromContext(ctx)

	logger.Info("provisioning nebius VM instance", "name", i.config.InstanceName)

	diskID, err := i.provisionBootDisk(ctx)
	if err != nil {
		return fmt.Errorf("provisioning boot disk: %w", err)
	}
	logger.Info("provisioned boot disk", "diskID", diskID)

	err = i.provisionInstance(ctx, diskID)
	if err != nil {
		return fmt.Errorf("provisioning VM instance: %w", err)
	}
	logger.Info("provisioned VM instance", "name", i.config.InstanceName)

	return nil
}

func (i *vmInstance) cleanupInstance(ctx context.Context) error {
	instance, err := i.getInstance(ctx, i.resolveInstanceMetadata())
	switch {
	case err == nil:
		// fallthrough to delete
	case isNotFound(err):
		// no need to delete
		return nil
	default:
		return fmt.Errorf("getting instance for cleanup: %w", err)
	}

	instanceService := i.sdk.Services().Compute().V1().Instance()
	deleteOp, err := instanceService.Delete(ctx, &nebiuscomputev1.DeleteInstanceRequest{
		Id: instance.Metadata.Id,
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting instance: %w", err)
	}
	_, err = deleteOp.Wait(ctx)
	if err != nil {
		return fmt.Errorf("waiting for instance deletion operation: %w", err)
	}
	return nil
}

func (i *vmInstance) cleanupBootDisk(ctx context.Context) error {
	disk, err := i.getBootDisk(ctx, i.resolveBootDiskMetadata())
	switch {
	case err == nil:
		// fallthrough to delete
	case isNotFound(err):
		// no need to delete
		return nil
	default:
		return fmt.Errorf("getting boot disk for cleanup: %w", err)
	}

	diskService := i.sdk.Services().Compute().V1().Disk()
	deleteOp, err := diskService.Delete(ctx, &nebiuscomputev1.DeleteDiskRequest{
		Id: disk.Metadata.Id,
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting boot disk: %w", err)
	}
	_, err = deleteOp.Wait(ctx)
	if err != nil {
		return fmt.Errorf("waiting for boot disk deletion operation: %w", err)
	}
	return nil
}

func (i *vmInstance) Cleanup(ctx context.Context) error {
	if err := i.cleanupInstance(ctx); err != nil {
		return fmt.Errorf("cleaning up instance: %w", err)
	}
	if err := i.cleanupBootDisk(ctx); err != nil {
		return fmt.Errorf("cleaning up boot disk: %w", err)
	}

	return nil
}

func deleteVMInstanceByNodeClaim(
	ctx context.Context,
	sdk *gosdk.SDK,
	nodeClaim *v1.NodeClaim,
) error {
	instanceID, err := providerIDToInstanceID(nodeClaim.Status.ProviderID)
	if err != nil {
		return fmt.Errorf("parsing providerID %q: %w", nodeClaim.Status.ProviderID, err)
	}
	instanceService := sdk.Services().Compute().V1().Instance()

	var bootDiskID string
	{
		instance, err := instanceService.Get(ctx, &nebiuscomputev1.GetInstanceRequest{
			Id: instanceID,
		})
		if err == nil {
			if instance.Spec.BootDisk != nil {
				bootDiskID = instance.Spec.BootDisk.GetExistingDisk().GetId()
			}
		}
	}

	deleteOp, err := instanceService.Delete(ctx, &nebiuscomputev1.DeleteInstanceRequest{
		Id: instanceID,
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting instance: %w", err)
	}
	_, err = deleteOp.Wait(ctx)
	if err != nil {
		return fmt.Errorf("waiting for instance deletion operation: %w", err)
	}

	if bootDiskID != "" {
		diskService := sdk.Services().Compute().V1().Disk()
		deleteDiskOp, err := diskService.Delete(ctx, &nebiuscomputev1.DeleteDiskRequest{
			Id: bootDiskID,
		})
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return fmt.Errorf("deleting boot disk: %w", err)
		}
		_, err = deleteDiskOp.Wait(ctx)
		if err != nil {
			return fmt.Errorf("waiting for boot disk deletion operation: %w", err)
		}
	}

	return nil
}

func getVMInstanceByProviderID(
	ctx context.Context,
	sdk *gosdk.SDK,
	providerID string,
) (*nebiuscomputev1.Instance, error) {
	instanceID, err := providerIDToInstanceID(providerID)
	if err != nil {
		return nil, fmt.Errorf("parsing providerID %q: %w", providerID, err)
	}
	instanceService := sdk.Services().Compute().V1().Instance()
	return instanceService.Get(ctx, &nebiuscomputev1.GetInstanceRequest{
		Id: instanceID,
	})
}

type vmInstanceLaunchPromise struct {
	kubeClient client.Client
	nodeClaim  *v1.NodeClaim
	instance   *vmInstance

	errChan      chan error
	cancelLaunch context.CancelFunc
}

var _ instance.Promise = (*vmInstanceLaunchPromise)(nil)

func launchVMInstance(
	kubeClient client.Client,
	nodeClaim *v1.NodeClaim,
	sdk *gosdk.SDK,
	instanceConfig *vmInstanceConfig,
) (*vmInstanceLaunchPromise, error) {
	const launchTimeout = 10 * time.Minute

	launchCtx, cancel := context.WithTimeout(context.Background(), launchTimeout)
	errChan := make(chan error, 1)

	rv := &vmInstanceLaunchPromise{
		kubeClient: kubeClient,
		nodeClaim:  nodeClaim,
		instance:   newVMInstance(instanceConfig, sdk),

		errChan:      errChan,
		cancelLaunch: cancel,
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				// print panic stack
				buf := make([]byte, 1<<16)
				runtime.Stack(buf, true)

				rv.errChan <- fmt.Errorf("panic during VM launch: %v\n%s", r, string(buf))
			}
		}()
		defer cancel()

		rv.errChan <- rv.instance.Provision(launchCtx)
	}()

	return rv, nil
}

func ignoreContextCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (p *vmInstanceLaunchPromise) Cleanup(ctx context.Context) error {
	p.cancelLaunch()

	p.cleanupNodeClaim(ctx)
	p.cleanupInstance(ctx)

	// NOTE: clean up is best effort; we proceed even if errors happen
	return nil
}

func (p *vmInstanceLaunchPromise) cleanupNodeClaim(ctx context.Context) {
	deleteErr := p.kubeClient.Delete(ctx, p.nodeClaim)
	deleteErr = client.IgnoreNotFound(deleteErr)
	deleteErr = ignoreContextCanceled(deleteErr)

	if deleteErr != nil {
		log.FromContext(ctx).Error(
			deleteErr,
			"cleaning up nodeClaim after failed instance launch",
			"nodeClaim", p.nodeClaim.Name,
		)
	}
}

func (p *vmInstanceLaunchPromise) cleanupInstance(ctx context.Context) {
	cleanupErr := p.instance.Cleanup(ctx)
	cleanupErr = ignoreContextCanceled(cleanupErr)

	if cleanupErr != nil {
		log.FromContext(ctx).Error(
			cleanupErr,
			"cleaning up instance after failed launch",
			"instance", p.GetInstanceName(),
		)
	}
}

func (p *vmInstanceLaunchPromise) GetInstanceName() string {
	return p.instance.config.InstanceName
}

func (p *vmInstanceLaunchPromise) Wait() error {
	return <-p.errChan
}

// FIXME: this method is added for resolving the vm instance id without waiting
// for the full provisioning to complete. In the long term, we should revisit
// the semantics of the "provider id" value in node claim.
func (p *vmInstanceLaunchPromise) PollInstance(ctx context.Context) (*nebiuscomputev1.Instance, error) {
	const pollInterval = 5 * time.Second

	logger := log.FromContext(ctx)

	instanceMetadata := p.instance.resolveInstanceMetadata()

	interval := time.NewTimer(pollInterval)
	defer interval.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-p.errChan:
			if err != nil {
				return nil, err
			}
		case <-interval.C:
			instance, err := p.instance.getInstance(ctx, instanceMetadata)
			if err != nil {
				if !isNotFound(err) {
					logger.Error(err, "poll instance id failed")
				}
				interval.Reset(pollInterval)
				continue
			}
			return instance, nil
		}
	}
}

func filterNoneZeroResource(
	_ corev1.ResourceName,
	v resource.Quantity,
) bool {
	return !resources.IsZero(v)
}

func nodeClaimFromInstance(
	instance *nebiuscomputev1.Instance,
	instanceType *cloudprovider.InstanceType,
) *v1.NodeClaim {
	rv := &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              strings.TrimPrefix(instance.Metadata.GetName(), instanceNamePrefix),
			Labels:            map[string]string{},
			Annotations:       map[string]string{},
			CreationTimestamp: metav1.NewTime(instance.Metadata.GetCreatedAt().AsTime()),
		},
		Spec: v1.NodeClaimSpec{},
		Status: v1.NodeClaimStatus{
			ProviderID: vmInstanceProviderID(instance.Metadata.GetId()),
		},
	}

	if instance.Status.State == nebiuscomputev1.InstanceStatus_DELETING {
		rv.DeletionTimestamp = lo.ToPtr(metav1.Now())
	}

	if instanceType != nil { // can this be nil at all?
		rv.Labels = labelspkg.GetAllSingleValuedRequirementLabels(instanceType.Requirements)
		rv.Status.Capacity = lo.PickBy(instanceType.Capacity, filterNoneZeroResource)
		rv.Status.Allocatable = lo.PickBy(instanceType.Allocatable(), filterNoneZeroResource)
	}

	// TODO: zone from instance
	// TODO: labels from instance

	return rv
}
