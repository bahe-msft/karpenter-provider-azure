package userdata

import (
	_ "embed"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/cluster-api/bootstrap/kubeadm/types/upstreamv1beta4"

	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/cloudinit"
)

var (
	s     = json.NewYAMLSerializer(json.DefaultMetaFactory, scheme.Scheme, scheme.Scheme)
	codec = scheme.Codecs.CodecForVersions(s, nil, schema.GroupVersions{upstreamv1beta4.GroupVersion}, nil)

	//go:embed assets/user-data
	userData []byte
)

func init() {
	scheme.Scheme.AddKnownTypes(upstreamv1beta4.GroupVersion, &upstreamv1beta4.JoinConfiguration{})
}

func UserData(
	opts *options.Options,
	aksClusterRestConfig *rest.Config,
	extraNodeLabels []string,
) (*cloudinit.UserData, error) {
	joinconfig, err := joinConfig(opts, extraNodeLabels)
	if err != nil {
		return nil, err
	}

	ud, err := cloudinit.Unmarshal(userData)
	if err != nil {
		return nil, err
	}

	bootstrapKubeconfig, err := bootstrapKubeConfig(opts, aksClusterRestConfig)
	if err != nil {
		return nil, err
	}

	ud.WriteFiles = append(ud.WriteFiles, &cloudinit.WriteFile{
		Path:        "/root/.kube/bootstrap-config",
		Content:     string(bootstrapKubeconfig),
		Permissions: "0600",
	}, &cloudinit.WriteFile{
		Path:    "/root/joinconfig",
		Content: string(joinconfig),
	})

	ud.RunCmd = append(ud.RunCmd,
		[]string{"kubeadm", "join", "--config", "/root/joinconfig"},
		[]string{"rm", "-rf", "/root/.kube", "/root/joinconfig"},
	)

	if opts.SSHPublicKey != "" {
		ud.SSHAuthorizedKeys = append(ud.SSHAuthorizedKeys, opts.SSHPublicKey)
	}

	return ud, nil
}

func bootstrapKubeConfig(
	opts *options.Options,
	aksClusterRestConfig *rest.Config,
) ([]byte, error) {
	return clientcmd.Write(api.Config{
		Clusters: map[string]*api.Cluster{
			"cluster": {
				CertificateAuthorityData: aksClusterRestConfig.CAData,
				Server:                   opts.ClusterEndpoint,
			},
		},
		Contexts: map[string]*api.Context{
			"context": {
				Cluster:  "cluster",
				AuthInfo: "user",
			},
		},
		CurrentContext: "context",
		AuthInfos: map[string]*api.AuthInfo{
			"user": {
				Token: opts.KubeletClientTLSBootstrapToken,
			},
		},
	})
}

func joinConfig(opts *options.Options, extraNodeLabels []string) ([]byte, error) {
	nodeLabelList := []string{
		"kubernetes.azure.com/managed=false",
		"kubernetes.azure.com/cluster=" + opts.NodeResourceGroup,
	}
	nodeLabelList = append(nodeLabelList, extraNodeLabels...)

	return runtime.Encode(codec, &upstreamv1beta4.JoinConfiguration{
		Discovery: upstreamv1beta4.Discovery{
			File: &upstreamv1beta4.FileDiscovery{
				KubeConfigPath: "/root/.kube/bootstrap-config",
			},
		},
		NodeRegistration: upstreamv1beta4.NodeRegistrationOptions{
			KubeletExtraArgs: []upstreamv1beta4.Arg{
				{
					Name:  "node-labels",
					Value: strings.Join(nodeLabelList, ","),
				},
			},
		},
	})
}
