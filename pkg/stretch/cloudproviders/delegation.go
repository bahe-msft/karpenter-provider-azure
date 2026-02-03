package cloudproviders

import (
	"context"
	"fmt"
	"iter"

	"github.com/awslabs/operatorpkg/status"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

type DelegatedCloudProvider struct {
	Default cloudprovider.CloudProvider
	ByKind  map[string]cloudprovider.CloudProvider
}

var _ cloudprovider.CloudProvider = (*DelegatedCloudProvider)(nil)

func New(d cloudprovider.CloudProvider) *DelegatedCloudProvider {
	return &DelegatedCloudProvider{
		Default: d,
		ByKind:  make(map[string]cloudprovider.CloudProvider),
	}
}

func (d *DelegatedCloudProvider) RegisterKind(kind string, cp cloudprovider.CloudProvider) {
	d.ByKind[kind] = cp
}

func (d *DelegatedCloudProvider) choose(ref *v1.NodeClassReference) cloudprovider.CloudProvider {
	if ref == nil {
		return d.Default
	}
	if cp, ok := d.ByKind[ref.Kind]; ok {
		return cp
	}
	return d.Default
}

func (d *DelegatedCloudProvider) eachCloudProvider() iter.Seq[cloudprovider.CloudProvider] {
	return func(yield func(cloudprovider.CloudProvider) bool) {
		if !yield(d.Default) {
			return
		}
		for _, cp := range d.ByKind {
			if !yield(cp) {
				return
			}
		}
	}
}

func (d *DelegatedCloudProvider) Create(ctx context.Context, nodeClaim *v1.NodeClaim) (*v1.NodeClaim, error) {
	return d.choose(nodeClaim.Spec.NodeClassRef).Create(ctx, nodeClaim)
}

func (d *DelegatedCloudProvider) Delete(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	return d.choose(nodeClaim.Spec.NodeClassRef).Delete(ctx, nodeClaim)
}

func (d *DelegatedCloudProvider) Get(ctx context.Context, providerID string) (*v1.NodeClaim, error) {
	// FIXME: should have a registry of providerIDs to cloudproviders
	for cp := range d.eachCloudProvider() {
		nodeClaim, err := cp.Get(ctx, providerID)
		if err == nil && nodeClaim != nil {
			return nodeClaim, nil
		}
	}
	return nil, fmt.Errorf("unsupported provider id: %q", providerID)
}

func (d *DelegatedCloudProvider) GetInstanceTypes(ctx context.Context, nodePool *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	return d.choose(nodePool.Spec.Template.Spec.NodeClassRef).GetInstanceTypes(ctx, nodePool)
}

func (d *DelegatedCloudProvider) GetSupportedNodeClasses() []status.Object {
	var rv []status.Object
	for cp := range d.eachCloudProvider() {
		rv = append(rv, cp.GetSupportedNodeClasses()...)
	}
	return rv
}

func (d *DelegatedCloudProvider) IsDrifted(ctx context.Context, nodeClaim *v1.NodeClaim) (cloudprovider.DriftReason, error) {
	return d.choose(nodeClaim.Spec.NodeClassRef).IsDrifted(ctx, nodeClaim)
}

func (d *DelegatedCloudProvider) List(ctx context.Context) ([]*v1.NodeClaim, error) {
	var rv []*v1.NodeClaim
	for cp := range d.eachCloudProvider() {
		nodeClaims, err := cp.List(ctx)
		if err != nil {
			return nil, err
		}
		rv = append(rv, nodeClaims...)
	}
	return rv, nil
}

func (d *DelegatedCloudProvider) Name() string {
	return "azure-stretch"
}

func (d *DelegatedCloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	// FIXME: investigate meaning&usage of this part
	return d.Default.RepairPolicies()
}
