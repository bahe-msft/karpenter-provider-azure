package nebius

import (
	"context"
	"fmt"

	"github.com/awslabs/operatorpkg/status"
	"github.com/nebius/gosdk"
	nebiuscomputev1 "github.com/nebius/gosdk/proto/nebius/compute/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instancetype"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/cloudproviders"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/options"
)

type CloudProvider struct {
	sdk *gosdk.SDK
}

var _ corecloudprovider.CloudProvider = (*CloudProvider)(nil)

func new(sdk *gosdk.SDK) *CloudProvider {
	return &CloudProvider{sdk: sdk}
}

func Register(
	d *cloudproviders.DelegatedCloudProvider,
	sdk *gosdk.SDK,
) {
	d.RegisterKind("StretchNebiusNodeClass", new(sdk))
}

func (c *CloudProvider) Create(context.Context, *v1.NodeClaim) (*v1.NodeClaim, error) {
	panic("unimplemented")
}

func (c *CloudProvider) Delete(context.Context, *v1.NodeClaim) error {
	panic("unimplemented")
}

func (c *CloudProvider) Get(context.Context, string) (*v1.NodeClaim, error) {
	panic("unimplemented")
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

	req := &nebiuscomputev1.ListPlatformsRequest{
		ParentId: projectID,
	}

	var rv []*corecloudprovider.InstanceType
	for item, err := range c.sdk.Services().Compute().V1().Platform().Filter(ctx, req) {
		if err != nil {
			return nil, fmt.Errorf("filter supported platforms from %q: %w", projectID, err)
		}

		for _, preset := range item.GetSpec().GetPresets() {
			logger.V(8).Info(
				"found nebius platform preset",
				"platform.id", item.GetMetadata().GetId(),
				"platform.name", item.GetMetadata().GetName(),
				"platform.human_readable_name", item.GetSpec().GetHumanReadableName(),
				"preset.name", preset.GetName(),
				"preset.vcpu_count", preset.GetResources().GetVcpuCount(),
				"preset.memory_gb", preset.GetResources().GetMemoryGibibytes(),
				"preset.gpu_count", preset.GetResources().GetGpuCount(),
			)

			vcpusCount := fmt.Sprint(preset.GetResources().GetVcpuCount())
			memoryGiB := fmt.Sprint(preset.GetResources().GetMemoryGibibytes())
			memoryMiB := fmt.Sprint(preset.GetResources().GetMemoryGibibytes() * 1024)
			gpuCount := fmt.Sprint(preset.GetResources().GetGpuCount())

			instanceType := &corecloudprovider.InstanceType{
				// FIXME: confirm naming convention
				Name: fmt.Sprintf("%s-%s", item.GetMetadata().GetName(), preset.GetName()),
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(
						corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Linux),
					),
					scheduling.NewRequirement(v1beta1.LabelSKUCPU, corev1.NodeSelectorOpIn, vcpusCount),
					scheduling.NewRequirement(v1beta1.LabelSKUMemory, corev1.NodeSelectorOpIn, memoryMiB),
					scheduling.NewRequirement(v1beta1.LabelSKUGPUCount, corev1.NodeSelectorOpIn, gpuCount),
				),
				Offerings: corecloudprovider.Offerings{
					{
						Price:     1000, // FIXME: calculate real price
						Available: true,
					},
				},
				Capacity: corev1.ResourceList{
					corev1.ResourceCPU:                    *resources.Quantity(vcpusCount),
					corev1.ResourceMemory:                 *resources.Quantity(memoryGiB),
					corev1.ResourceEphemeralStorage:       *resource.NewScaledQuantity(100, resource.Giga), // FIXME: read from node class
					corev1.ResourcePods:                   *resources.Quantity("110"),                      // FIXME: read from node class
					corev1.ResourceName("nvidia.com/gpu"): *resources.Quantity(gpuCount),
				},
				Overhead: &corecloudprovider.InstanceTypeOverhead{
					KubeReserved: instancetype.KubeReservedResources(
						int64(preset.Resources.VcpuCount),
						float64(preset.Resources.MemoryGibibytes),
					),
					SystemReserved: corev1.ResourceList{
						corev1.ResourceCPU:    resource.Quantity{},
						corev1.ResourceMemory: resource.Quantity{},
					},
					EvictionThreshold: instancetype.EvictionThreshold(),
				},
			}

			rv = append(rv, instanceType)
		}
	}

	return rv, nil
}

func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{
		&v1beta1.StretchNebiusNodeClass{},
	}
}

func (c *CloudProvider) IsDrifted(context.Context, *v1.NodeClaim) (corecloudprovider.DriftReason, error) {
	panic("unimplemented")
}

func (c *CloudProvider) List(context.Context) ([]*v1.NodeClaim, error) {
	// TODO: list nebius VMs and map to NodeClaims
	return []*v1.NodeClaim{}, nil
}

func (c *CloudProvider) Name() string {
	return "azure-stretch-nebius"
}

func (c *CloudProvider) RepairPolicies() []corecloudprovider.RepairPolicy {
	return nil
}
