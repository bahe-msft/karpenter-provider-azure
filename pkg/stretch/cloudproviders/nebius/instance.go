package nebius

import (
	"context"
	"fmt"

	"github.com/nebius/gosdk"
	"github.com/samber/lo"
	"k8s.io/client-go/rest"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	stretchoptions "github.com/Azure/karpenter-provider-azure/pkg/stretch/options"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/userdata"
)

type vmInstanceConfig struct {
	ProjectID     string
	Name          string
	Platform      string // e.g., "cpu-d3", "cpu-e2"
	Preset        string // e.g., "4vcpu-16gb"
	SubnetID      string
	ImageFamily   string // e.g., "ubuntu24.04-driverless"
	DiskSizeGB    int32
	SSHKey        string
	CloudInitData string // cloud-init user data
}

func newVMInstanceConfig(
	ctx context.Context,
	nodeClass *v1beta1.StretchNebiusNodeClass,
	nodeClaim *v1.NodeClaim,
	platformPreset *platformPreset,
	aksClusterRestConfig *rest.Config,
) (vmInstanceConfig, error) {
	karpOpts := options.FromContext(ctx) // FIXME: this pattern is not great
	userData, err := userdata.UserData(karpOpts, aksClusterRestConfig)
	if err != nil {
		return vmInstanceConfig{}, fmt.Errorf("generating user data: %w", err)
	}
	userDataEncoded, err := userData.Marshal()
	if err != nil {
		return vmInstanceConfig{}, fmt.Errorf("marshaling user data: %w", err)
	}

	rv := vmInstanceConfig{
		ProjectID: stretchoptions.MustGetNebiusProjectID(ctx), // FIXME: maybe resolve from node class?
		// FIXME: confirm naming pattern in nebius side
		Name:     fmt.Sprintf("stretch-nebius-%s", nodeClaim.Name),
		Platform: platformPreset.platform.GetMetadata().GetName(),
		Preset:   platformPreset.preset.GetName(),
		SubnetID: nodeClass.Spec.SubnetID,
		// FIXME: resolve default image from nebius instead
		ImageFamily:   lo.FromPtrOr(nodeClass.Spec.OSDiskImageFamily, "ubuntu24.04-driverless"),
		DiskSizeGB:    lo.FromPtrOr(nodeClass.Spec.OSDiskSizeGB, int32(100)),
		SSHKey:        karpOpts.SSHPublicKey,
		CloudInitData: string(userDataEncoded),
	}

	// TODO: validate settings values

	return rv, nil
}

type vmInstance struct {
	config vmInstanceConfig
	sdk    *gosdk.SDK

	diskID     string
	instanceID string
}

func newVMInstance(
	instanceConfig vmInstanceConfig,
	sdk *gosdk.SDK,
) *vmInstance {
	return &vmInstance{
		sdk:    sdk,
		config: instanceConfig,
	}
}
