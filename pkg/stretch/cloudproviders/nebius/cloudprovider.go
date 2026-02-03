package nebius

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/awslabs/operatorpkg/status"
	"github.com/nebius/gosdk"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/Azure/karpenter-provider-azure/pkg/apis"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/cloudproviders"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/options"
	"github.com/Azure/karpenter-provider-azure/pkg/utils"
)

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
	logger.Info("resolved platform preset for launching instance", "platformPreset", platformPresetToLaunch.InstanceTypeName())

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
	instance := newVMInstance(instanceConfig, c.sdk)
	_ = instance

	b, _ := json.MarshalIndent(instanceConfig, "", "  ")
	fmt.Println("instanceConfig:\n", string(b))

	// populate nodeClaim status to bookkeep the created VM (provider ID)

	panic("create unimplemented")
}

func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	panic("delete unimplemented")
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*v1.NodeClaim, error) {
	panic("get unimplemented")
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
	panic("isDrifted unimplemented")
}

func (c *CloudProvider) List(ctx context.Context) ([]*v1.NodeClaim, error) {
	// TODO: list nebius VMs and map to NodeClaims
	return []*v1.NodeClaim{}, nil
}

func (c *CloudProvider) Name() string {
	return "azure-stretch-nebius"
}

func (c *CloudProvider) RepairPolicies() []corecloudprovider.RepairPolicy {
	return nil
}
