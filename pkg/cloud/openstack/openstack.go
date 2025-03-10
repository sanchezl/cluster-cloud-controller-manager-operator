package openstack

import (
	"bytes"
	"embed"
	"fmt"

	"github.com/asaskevich/govalidator"
	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	ini "gopkg.in/ini.v1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/cluster-cloud-controller-manager-operator/pkg/cloud/common"
	"github.com/openshift/cluster-cloud-controller-manager-operator/pkg/config"
)

const providerName = "openstack"

var (
	//go:embed assets/*
	assetsFs embed.FS

	templates = []common.TemplateSource{
		{ReferenceObject: &appsv1.Deployment{}, EmbedFsPath: "assets/deployment.yaml"},
	}
)

type imagesReference struct {
	CloudControllerManager string `valid:"required"`
}

var templateValuesValidationMap = map[string]interface{}{
	"images":            "required",
	"cloudproviderName": "required,type(string)",
	"featureGates":      "type(string)",
}

type openstackAssets struct {
	operatorConfig    config.OperatorConfig
	renderedResources []client.Object
}

func (o *openstackAssets) GetRenderedResources() []client.Object {
	return o.renderedResources
}

func getTemplateValues(images *imagesReference, operatorConfig config.OperatorConfig) (common.TemplateValues, error) {
	values := common.TemplateValues{
		"images":            images,
		"cloudproviderName": operatorConfig.GetPlatformNameString(),
		"featureGates":      operatorConfig.FeatureGates,
	}
	_, err := govalidator.ValidateMap(values, templateValuesValidationMap)
	if err != nil {
		return nil, err
	}
	return values, nil
}

// createLoadBalancerSection creates a loadBalancer section populated with
// the use-octavia key and depending on the network type also the enabled key.
// It returns any error that happens.
func createLoadBalancerSection(cfg *ini.File, network *configv1.Network) error {
	loadBalancer, err := cfg.NewSection("LoadBalancer")
	if err != nil {
		return fmt.Errorf("failed to modify the provided configuration: %w", err)
	}
	_, err = loadBalancer.NewKey("use-octavia", "true")
	if err != nil {
		return fmt.Errorf("failed to modify the provided configuration: %w", err)
	}
	if network.Spec.NetworkType == string(operatorv1.NetworkTypeKuryr) {
		_, err = loadBalancer.NewKey("enabled", "false")
		if err != nil {
			return fmt.Errorf("failed to modify the provided configuration: %w", err)
		}
	}
	return nil
}

// updateLoadBalancerSection updates the loadBalancer section with the use-octavia key
// and and depending on the network type also the enabled key. It returns any error that happens.
func updateLoadBalancerSection(loadBalancer *ini.Section, network *configv1.Network) error {
	useOctaviaKey, err := loadBalancer.GetKey("use-octavia")
	if err != nil {
		_, err = loadBalancer.NewKey("use-octavia", "true")
		if err != nil {
			return fmt.Errorf("failed to modify the provided configuration: %w", err)
		}
	} else {
		useOctaviaKey.SetValue("true")
	}
	if network.Spec.NetworkType == string(operatorv1.NetworkTypeKuryr) {
		enabledKey, err := loadBalancer.GetKey("enabled")
		if err != nil {
			_, err = loadBalancer.NewKey("enabled", "false")
			if err != nil {
				return fmt.Errorf("failed to modify the provided configuration: %w", err)
			}
		} else {
			enabledKey.SetValue("false")
		}
	}
	return nil
}

func NewProviderAssets(config config.OperatorConfig) (common.CloudProviderAssets, error) {
	images := &imagesReference{
		CloudControllerManager: config.ImagesReference.CloudControllerManagerOpenStack,
	}
	_, err := govalidator.ValidateStruct(images)
	if err != nil {
		return nil, fmt.Errorf("%s: missed images in config: %v", providerName, err)
	}
	assets := &openstackAssets{
		operatorConfig: config,
	}
	objTemplates, err := common.ReadTemplates(assetsFs, templates)
	if err != nil {
		return nil, err
	}
	templateValues, err := getTemplateValues(images, config)
	if err != nil {
		return nil, fmt.Errorf("can not construct template values for %s assets: %v", providerName, err)
	}

	assets.renderedResources, err = common.RenderTemplates(objTemplates, templateValues)
	if err != nil {
		return nil, err
	}

	return assets, nil
}

// CloudConfigTransformer implements the cloudConfigTransformer. It takes
// the user-provided, legacy cloud provider-compatible configuration and
// modifies it to be compatible with the external cloud provider. It returns
// an error if the platform is not OpenStackPlatformType or if any errors are
// encountered while attempting to rework the configuration.
func CloudConfigTransformer(source string, infra *configv1.Infrastructure, network *configv1.Network) (string, error) {
	if infra.Status.PlatformStatus == nil ||
		infra.Status.PlatformStatus.Type != configv1.OpenStackPlatformType {
		return "", fmt.Errorf("invalid platform, expected to be %s", configv1.OpenStackPlatformType)
	}

	cfg, err := ini.Load([]byte(source))
	if err != nil {
		return "", fmt.Errorf("failed to read the cloud.conf: %w", err)
	}

	global, _ := cfg.GetSection("Global")
	if global != nil {
		klog.Infof("[Global] section found; dropping any legacy settings...")
		// Remove the legacy keys, once we ensure they're not overridden
		for key, value := range map[string]string{
			"secret-name":      "openstack-credentials",
			"secret-namespace": "kube-system",
			"kubeconfig-path":  "",
		} {
			if global.Key(key).String() != value {
				return "", fmt.Errorf("'[Global] %s' is set to a non-default value", key)
			}
			global.DeleteKey(key)
		}
	} else {
		// Section doesn't exist, ergo no validation to concern ourselves with.
		// This probably isn't common but at least handling this allows us to
		// recover gracefully
		global, err = cfg.NewSection("Global")
		if err != nil {
			return "", fmt.Errorf("failed to modify the provided configuration: %w", err)
		}
	}

	// Use a slice to preserve keys order
	for _, o := range []struct{ k, v string }{
		{"use-clouds", "true"},
		{"clouds-file", "/etc/openstack/secret/clouds.yaml"},
		{"cloud", "openstack"},
	} {
		_, err = global.NewKey(o.k, o.v)
		if err != nil {
			return "", fmt.Errorf("failed to modify the provided configuration: %w", err)
		}
	}

	blockStorage, _ := cfg.GetSection("BlockStorage")
	if blockStorage != nil {
		klog.Infof("[BlockStorage] section found; dropping section...")
		cfg.DeleteSection("BlockStorage")
	}

	loadBalancer, _ := cfg.GetSection("LoadBalancer")
	if loadBalancer == nil {
		if err = createLoadBalancerSection(cfg, network); err != nil {
			return "", fmt.Errorf("could not create load balancer section: %w", err)
		}
	} else {
		if err = updateLoadBalancerSection(loadBalancer, network); err != nil {
			return "", fmt.Errorf("could not update load balancer section: %w", err)
		}
	}

	var buf bytes.Buffer

	_, err = cfg.WriteTo(&buf)
	if err != nil {
		return "", fmt.Errorf("failed to modify the provided configuration: %w", err)
	}

	return buf.String(), nil
}
