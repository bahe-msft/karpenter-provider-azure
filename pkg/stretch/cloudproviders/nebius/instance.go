package nebius

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/nebius/gosdk"
	nebiuscommonv1 "github.com/nebius/gosdk/proto/nebius/common/v1"
	nebiuscomputev1 "github.com/nebius/gosdk/proto/nebius/compute/v1"
	"github.com/samber/lo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/cloudproviders"
	stretchoptions "github.com/Azure/karpenter-provider-azure/pkg/stretch/options"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/userdata"
)

const (
	providerIDInstancePrefix = "stretch-nebius://instance/"

	resourceLabelKeyManagedBy   = "karpenter.azure.com/managed-by"
	resourceLabelValueManagedBy = "stretch-nebius"
	resourceLabelKeyOwnedBy     = "karpenter.azure.com/owned-by"
)

func vmInstanceProviderID(instanceID string) string {
	return providerIDInstancePrefix + instanceID
}

func providerIDToInstanceID(providerID string) (string, error) {
	prefix := providerIDInstancePrefix
	if !strings.HasPrefix(providerID, prefix) {
		return "", fmt.Errorf("invalid providerID scheme, expected prefix %q", prefix)
	}
	instanceID := strings.TrimPrefix(providerID, prefix)

	return instanceID, nil
}

const instanceNamePrefix = "stretch-nebius-"

type vmInstanceConfig struct {
	ProjectID            string
	ClusterID            string // AKS cluster id
	InstanceName         string
	Platform             string // e.g., "cpu-d3", "cpu-e2"
	Preset               string // e.g., "4vcpu-16gb"
	SubnetID             string
	ImageFamily          string // e.g., "ubuntu24.04-driverless"
	DiskSizeGB           int32
	CloudInitData        string // cloud-init user data
	AllocateNodePublicIP bool   // whether to assign a public IP to the node
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
		ClusterID: karpOpts.ClusterID,
		// FIXME: confirm naming pattern in nebius side
		InstanceName: fmt.Sprintf("%s%s", instanceNamePrefix, nodeClaim.Name),
		Platform:     platformPreset.platform.GetMetadata().GetName(),
		Preset:       platformPreset.preset.GetName(),
		SubnetID:     nodeClass.Spec.SubnetID,
		// FIXME: resolve default image from nebius instead
		ImageFamily:          lo.FromPtrOr(nodeClass.Spec.OSDiskImageFamily, "ubuntu24.04-driverless"),
		DiskSizeGB:           lo.FromPtrOr(nodeClass.Spec.OSDiskSizeGB, int32(100)),
		CloudInitData:        string(userDataEncoded),
		AllocateNodePublicIP: lo.FromPtrOr(nodeClass.Spec.AllocateNodePublicIP, false),
	}

	// TODO: validate settings values

	return rv, nil
}

type vmInstanceOperator struct {
	logger logr.Logger
	sdk    *gosdk.SDK
	config *vmInstanceConfig

	bootDiskMetadata *nebiuscommonv1.ResourceMetadata
	instanceMetadata *nebiuscommonv1.ResourceMetadata
}

func newVMInstanceOperator(
	logger logr.Logger,
	sdk *gosdk.SDK,
	instanceConfig *vmInstanceConfig,
) *vmInstanceOperator {
	return &vmInstanceOperator{
		logger: logger,
		sdk:    sdk,
		config: instanceConfig,

		bootDiskMetadata: &nebiuscommonv1.ResourceMetadata{
			ParentId: instanceConfig.ProjectID,
			Name:     fmt.Sprintf("%s-boot-disk", instanceConfig.InstanceName),
			Labels: map[string]string{
				resourceLabelKeyManagedBy: resourceLabelValueManagedBy,
				resourceLabelKeyOwnedBy:   instanceConfig.ClusterID,
			},
		},
		instanceMetadata: &nebiuscommonv1.ResourceMetadata{
			ParentId: instanceConfig.ProjectID,
			Name:     instanceConfig.InstanceName,
			Labels: map[string]string{
				resourceLabelKeyManagedBy: resourceLabelValueManagedBy,
				resourceLabelKeyOwnedBy:   instanceConfig.ClusterID,
			},
		},
	}
}

func (i *vmInstanceOperator) getBootDisk(ctx context.Context) (*nebiuscomputev1.Disk, error) {
	diskService := i.sdk.Services().Compute().V1().Disk()
	disk, err := diskService.GetByName(ctx, &nebiuscommonv1.GetByNameRequest{
		ParentId: i.bootDiskMetadata.ParentId,
		Name:     i.bootDiskMetadata.Name,
	})
	return notFoundIfNotManaged(ctx, disk, err)
}

func (i *vmInstanceOperator) provisionBootDisk(
	ctx context.Context,
) (string, error) {
	disk, err := i.getBootDisk(ctx)
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
		Metadata: i.bootDiskMetadata,
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
			disk, getErr := i.getBootDisk(ctx)
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

	diskID := op.ResourceID()

	return diskID, nil
}

func (i *vmInstanceOperator) getInstance(ctx context.Context) (*nebiuscomputev1.Instance, error) {
	instanceService := i.sdk.Services().Compute().V1().Instance()
	instance, err := instanceService.GetByName(ctx, &nebiuscommonv1.GetByNameRequest{
		ParentId: i.instanceMetadata.ParentId,
		Name:     i.instanceMetadata.Name,
	})
	return notFoundIfNotManaged(ctx, instance, err)
}

func (i *vmInstanceOperator) launchInstance(
	ctx context.Context,
	bootDiskID string,
) (AsyncOperation, error) {
	instanceService := i.sdk.Services().Compute().V1().Instance()
	_, err := i.getInstance(ctx)
	switch {
	case err == nil:
		// instance already exists
		return &completedOperation{}, nil
	case isNotFound(err):
		// not found, proceed to create
	default:
		return nil, fmt.Errorf("checking for existing instance: %w", err)
	}

	primaryNIC := &nebiuscomputev1.NetworkInterfaceSpec{
		SubnetId:  i.config.SubnetID,
		Name:      "eth0",
		IpAddress: &nebiuscomputev1.IPAddress{
			// Auto-allocate private IP
		},
	}
	if i.config.AllocateNodePublicIP {
		primaryNIC.PublicIpAddress = &nebiuscomputev1.PublicIPAddress{
			// Auto-allocate public IP
		}
	}

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
			primaryNIC,
		},
		CloudInitUserData: i.config.CloudInitData,
	}

	op, err := instanceService.Create(ctx, &nebiuscomputev1.CreateInstanceRequest{
		Metadata: i.instanceMetadata,
		Spec:     spec,
	})
	if err != nil {
		return nil, err
	}
	return newAsyncOperationFromNebius(op), nil
}

func (i *vmInstanceOperator) Launch(ctx context.Context) (AsyncOperation, error) {
	diskID, err := i.provisionBootDisk(ctx)
	if err != nil {
		return nil, fmt.Errorf("provisioning boot disk: %w", err)
	}

	op, err := i.launchInstance(ctx, diskID)
	if err != nil {
		return nil, fmt.Errorf("launching VM instance: %w", err)
	}

	return op, nil
}

func (i *vmInstanceOperator) cleanupInstance(ctx context.Context) error {
	instance, err := i.getInstance(ctx)
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

func (i *vmInstanceOperator) cleanupBootDisk(ctx context.Context) error {
	disk, err := i.getBootDisk(ctx)
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

func (i *vmInstanceOperator) Cleanup(ctx context.Context) error {
	if err := i.cleanupInstance(ctx); err != nil {
		return fmt.Errorf("cleaning up instance: %w", err)
	}
	if err := i.cleanupBootDisk(ctx); err != nil {
		return fmt.Errorf("cleaning up boot disk: %w", err)
	}

	return nil
}

func (i *vmInstanceOperator) LaunchInBackground(
	kubeClient client.Client,
	nodeClaim *v1.NodeClaim,
) error {
	cleanUpBestEffort := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		// TODO: confirm if we need to wait until node claim is being set with launched condition
		{
			err := kubeClient.Delete(ctx, nodeClaim)
			err = client.IgnoreNotFound(err)
			if err != nil {
				i.logger.Error(err, "cleaning up nodeClaim after failed instance launch")
			} else {
				i.logger.V(5).Info("cleaned up nodeClaim after failed instance launch")
			}
		}

		{
			err := i.Cleanup(ctx)
			if err != nil {
				i.logger.Error(err, "cleaning up instance after failed launch")
			} else {
				i.logger.V(5).Info("cleaned up instance after failed launch")
			}
		}
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicStack := make([]byte, 1<<16)
				runtime.Stack(panicStack, true)
				i.logger.Error(
					fmt.Errorf("panic during VM launch: %v\n%s", r, string(panicStack)),
					"launching VM instance",
				)
				cleanUpBestEffort()
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		op, err := i.Launch(ctx)
		if err != nil {
			cleanUpBestEffort()
			return
		}

		// FIXME: do we still want to clean up in this case?
		err = op.Wait(ctx)
		if err != nil {
			cleanUpBestEffort()
			return
		}
	}()

	return nil
}

func (i *vmInstanceOperator) WaitUntilAcceptedByRemote(
	ctx context.Context,
) (*nebiuscomputev1.Instance, error) {
	const pollInterval = 5 * time.Second

	interval := time.NewTimer(pollInterval)
	defer interval.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-interval.C:
			instance, err := i.getInstance(ctx)
			switch {
			case err == nil:
				// NOTE: treating DELETING state as not found so we can clean up the node claim early
				if instance.GetStatus().GetState() == nebiuscomputev1.InstanceStatus_DELETING {
					return nil, fmt.Errorf("instance %q is being deleted", instance.GetMetadata().GetId())
				}
				return instance, nil
			case isNotFound(err):
				interval.Reset(pollInterval)
				continue
			case err != nil:
				return nil, err
			}
		}
	}
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
	instance, err := instanceService.Get(ctx, &nebiuscomputev1.GetInstanceRequest{
		Id: instanceID,
	})
	return notFoundIfNotManaged(ctx, instance, err)
}

func deleteVMInstanceByProviderID(
	ctx context.Context,
	logger logr.Logger,
	sdk *gosdk.SDK,
	providerID string,
) error {
	instance, err := getVMInstanceByProviderID(ctx, sdk, providerID)
	switch {
	case isNotFound(err):
		// not found, no op
		return nil
	case err != nil:
		return fmt.Errorf("getting instance before deletion: %w", err)
	default:
		// fallthrough to delete
	}

	bootDiskID := instance.GetSpec().GetBootDisk().GetExistingDisk().GetId()

	{
		instanceService := sdk.Services().Compute().V1().Instance()
		instanceID := instance.GetMetadata().GetId()
		deleteOp, err := instanceService.Delete(ctx, &nebiuscomputev1.DeleteInstanceRequest{
			Id: instanceID,
		})
		if err := ignoreNotFound(err); err != nil {
			return fmt.Errorf("delete instance %q: %w", instanceID, err)
		}
		if _, err := deleteOp.Wait(ctx); err != nil {
			return fmt.Errorf("waiting for instance deletion operation: %w", err)
		}
		logger.Info("deleted VM instance", "instanceID", instanceID)
	}

	if bootDiskID != "" {
		diskService := sdk.Services().Compute().V1().Disk()
		deleteDiskOp, err := diskService.Delete(ctx, &nebiuscomputev1.DeleteDiskRequest{
			Id: bootDiskID,
		})
		if err := ignoreNotFound(err); err != nil {
			return fmt.Errorf("delete boot disk %q: %w", bootDiskID, err)
		}
		if _, err := deleteDiskOp.Wait(ctx); err != nil {
			return fmt.Errorf("waiting for boot disk deletion operation: %w", err)
		}
		logger.Info("deleted boot disk", "diskID", bootDiskID)
	}

	return nil
}

func filterNoneZeroResource(
	_ corev1.ResourceName,
	v resource.Quantity,
) bool {
	return !resources.IsZero(v)
}

func isManagedResource(
	ctx context.Context,
	md *nebiuscommonv1.ResourceMetadata,
) bool {
	labels := md.GetLabels()
	clusterID := options.FromContext(ctx).ClusterID // FIXME: this pattern is not great
	if labels[resourceLabelKeyManagedBy] == resourceLabelValueManagedBy &&
		labels[resourceLabelKeyOwnedBy] == clusterID {
		return true
	}

	return false
}

type withResourceMetadata interface {
	GetMetadata() *nebiuscommonv1.ResourceMetadata
}

// FIXME: make it as a grpc interceptor instead
func notFoundIfNotManaged[T withResourceMetadata](
	ctx context.Context,
	res T, err error,
) (T, error) {
	if err != nil {
		return res, err
	}
	md := res.GetMetadata()
	if !isManagedResource(ctx, md) {
		err := status.Errorf(codes.NotFound, "resource %q is not managed by this cluster", md)
		log.FromContext(ctx).Error(err, "resource not managed by this cluster")
		return res, err
	}
	return res, nil
}
