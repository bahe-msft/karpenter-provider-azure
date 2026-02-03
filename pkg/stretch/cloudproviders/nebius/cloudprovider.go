package nebius

import (
	"context"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/cloudproviders"
	"github.com/awslabs/operatorpkg/status"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

type CloudProvider struct{}

var _ corecloudprovider.CloudProvider = (*CloudProvider)(nil)

func New() *CloudProvider {
	return &CloudProvider{}
}

func Register(
	d *cloudproviders.DelegatedCloudProvider,
) {
	d.RegisterKind("StretchNebiusNodeClass", New())
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

func (c *CloudProvider) GetInstanceTypes(context.Context, *v1.NodePool) ([]*corecloudprovider.InstanceType, error) {
	panic("unimplemented")
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
