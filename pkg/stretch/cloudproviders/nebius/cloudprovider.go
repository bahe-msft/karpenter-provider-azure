package nebius

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/nebius/gosdk"
	nebiuscomputev1 "github.com/nebius/gosdk/proto/nebius/compute/v1"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/Azure/karpenter-provider-azure/pkg/apis"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	labelspkg "github.com/Azure/karpenter-provider-azure/pkg/providers/labels"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/cloudproviders"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/options"
	"github.com/Azure/karpenter-provider-azure/pkg/utils"
)

const (
	providerScheme           = "stretch-nebius"
	providerIDInstancePrefix = providerScheme + "://instance/"

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

type CloudProvider struct {
	sdk        *gosdk.SDK
	kubeClient client.Client
	restConfig *rest.Config
}

var _ corecloudprovider.CloudProvider = (*CloudProvider)(nil)

func new(
	sdk *gosdk.SDK,
	kubeClient client.Client,
	restConfig *rest.Config,
) *CloudProvider {
	return &CloudProvider{
		sdk:        sdk,
		kubeClient: kubeClient,
		restConfig: restConfig,
	}
}

func Register(
	d *cloudproviders.DelegatedCloudProvider,
	sdk *gosdk.SDK,
	kubeClient client.Client,
	restConfig *rest.Config,
) {
	d.RegisterKind("StretchNebiusNodeClass", new(sdk, kubeClient, restConfig))
}

func (c *CloudProvider) getNodeClass( // TODO: make it reusable
	ctx context.Context,
	nodeClaim *v1.NodeClaim,
) (*v1beta1.StretchNebiusNodeClass, error) {
	if nodeClaim.Spec.NodeClassRef == nil {
		return nil, fmt.Errorf("nodeClaim %s does not have a nodeClassRef", nodeClaim.Name)
	}

	rv := &v1beta1.StretchNebiusNodeClass{}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: nodeClaim.Spec.NodeClassRef.Name}, rv); err != nil {
		return nil, fmt.Errorf("getting StretchNebiusNodeClass %s: %w", nodeClaim.Spec.NodeClassRef.Name, err)
	}

	if !rv.DeletionTimestamp.IsZero() {
		return nil, utils.NewTerminatingResourceError(schema.GroupResource{Group: apis.Group, Resource: "nebiusnodeclass"}, rv.Name)
	}

	return rv, nil
}

func (c *CloudProvider) Create(ctx context.Context, nodeClaim *v1.NodeClaim) (*v1.NodeClaim, error) {
	logger := log.FromContext(ctx).WithValues("nodeClaim", nodeClaim.Name)
	logger.Info("creating nebius VM for nodeClaim")

	nodeClass, err := c.getNodeClass(ctx, nodeClaim)
	if err != nil {
		// FIXME: proper error attribution
		return nil, err
	}

	// resolve instance type to use based on pricing/offerings
	platformPresetToLaunch, err := resolvePlatformPresetFromNodeClaim(
		ctx,
		options.MustGetNebiusProjectID(ctx), // TODO: maybe resolve from node class?
		c.sdk,
		nodeClaim,
	)
	if err != nil {
		return nil, err
	}
	logger.Info(
		"resolved platform preset for launching instance",
		"platformPreset", platformPresetToLaunch.InstanceTypeName(),
	)

	// launch nebius instance
	instanceConfig, err := newVMInstanceConfig(
		ctx,
		nodeClass,
		nodeClaim,
		platformPresetToLaunch,
		c.restConfig,
	)
	if err != nil {
		return nil, err
	}

	launchInstancePromise, err := launchVMInstance(c.kubeClient, nodeClaim, c.sdk, instanceConfig)
	if err != nil {
		return nil, fmt.Errorf("launching VM instance: %w", err)
	}
	go func() {
		err := launchInstancePromise.Wait()
		if err == nil {
			// no need to clean up
			return
		}

		// FIXME: wait for node claim is set with launched condition

		logger.Error(err, "failed to launch nebius VM, cleaning up")
		cleanUpCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := launchInstancePromise.Cleanup(cleanUpCtx); err != nil {
			logger.Error(err, "failed to clean up nebius VM after launch failure")
		} else {
			logger.Info("successfully cleaned up nebius VM after launch failure")
		}
	}()

	// rebuild node claim object to reflect the launched instance
	instance, err := launchInstancePromise.PollInstance(ctx)
	if err != nil {
		return nil, fmt.Errorf("polling launched instance: %w", err)
	}
	instanceType := platformPresetToLaunch.ToInstanceType()

	newNodeClaim := nodeClaimFromInstance(instance, instanceType)
	// TODO: figure out meaning
	newNodeClaim.Labels = lo.Assign(
		newNodeClaim.Labels,
		labelspkg.GetWellKnownSingleValuedRequirementLabels(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)),
	)

	return newNodeClaim, nil
}

func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	return deleteVMInstanceByNodeClaim(ctx, c.sdk, nodeClaim)
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*v1.NodeClaim, error) {
	instance, err := getVMInstanceByProviderID(ctx, c.sdk, providerID)
	if err != nil {
		return nil, err
	}
	platformPreset, err := resolvePlatformPresetFromInstance(
		ctx,
		options.MustGetNebiusProjectID(ctx), // TODO: maybe resolve from node class?
		c.sdk,
		instance,
	)
	if err != nil {
		return nil, err
	}

	nodeClaim := nodeClaimFromInstance(instance, platformPreset.ToInstanceType())
	return nodeClaim, nil
}

func (c *CloudProvider) GetInstanceTypes(
	ctx context.Context,
	nodePool *v1.NodePool,
) ([]*corecloudprovider.InstanceType, error) {
	// TODO: proper caching design

	projectID := options.MustGetNebiusProjectID(ctx) // TODO: maybe resolve from node class?
	logger := log.FromContext(ctx).
		WithName("nebius-cloud-provider").
		WithValues("nebius.project-id", projectID)

	var rv []*corecloudprovider.InstanceType
	for platformPreset, err := range filterPlatformPresets(ctx, projectID, c.sdk) {
		if err != nil {
			return nil, fmt.Errorf("filter supported platforms from %q: %w", projectID, err)
		}
		platform := platformPreset.platform
		preset := platformPreset.preset
		logger.V(8).Info(
			"found nebius platform preset",
			"platform.id", platform.GetMetadata().GetId(),
			"platform.name", platform.GetMetadata().GetName(),
			"platform.human_readable_name", platform.GetSpec().GetHumanReadableName(),
			"preset.name", preset.GetName(),
			"preset.vcpu_count", preset.GetResources().GetVcpuCount(),
			"preset.memory_gb", preset.GetResources().GetMemoryGibibytes(),
			"preset.gpu_count", preset.GetResources().GetGpuCount(),
		)

		rv = append(rv, platformPreset.ToInstanceType())
	}

	return rv, nil
}

func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{
		&v1beta1.StretchNebiusNodeClass{},
	}
}

func (c *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *v1.NodeClaim) (corecloudprovider.DriftReason, error) {
	// TODO: implement drift detection
	return "", nil
}

func (c *CloudProvider) List(ctx context.Context) ([]*v1.NodeClaim, error) {
	var rv []*v1.NodeClaim

	projectID := options.MustGetNebiusProjectID(ctx) // TODO: maybe resolve from node class?
	instanceService := c.sdk.Services().Compute().V1().Instance()
	listReq := &nebiuscomputev1.ListInstancesRequest{
		ParentId: projectID,
	}
	for instance, err := range instanceService.Filter(ctx, listReq) {
		if err != nil {
			return nil, err
		}
		if !isManagedResource(ctx, instance.GetMetadata()) {
			continue
		}

		// FIXME: don't do this n+1 lookup
		// cache platform preset results
		platformPreset, err := resolvePlatformPresetFromInstance(
			ctx,
			projectID,
			c.sdk,
			instance,
		)
		if err != nil {
			return nil, err
		}

		nodeClaim := nodeClaimFromInstance(instance, platformPreset.ToInstanceType())
		rv = append(rv, nodeClaim)
	}

	return rv, nil
}

func (c *CloudProvider) Name() string {
	return "azure-stretch-nebius"
}

func (c *CloudProvider) RepairPolicies() []corecloudprovider.RepairPolicy {
	return nil
}
