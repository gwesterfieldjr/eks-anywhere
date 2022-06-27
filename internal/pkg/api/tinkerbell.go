package api

import (
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	anywherev1 "github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	"github.com/aws/eks-anywhere/pkg/cluster"
	"github.com/aws/eks-anywhere/pkg/providers"
	"github.com/aws/eks-anywhere/pkg/templater"
	"github.com/aws/eks-anywhere/pkg/version"
)

type TinkerbellConfig struct {
	clusterConfig    *anywherev1.Cluster
	datacenterConfig *anywherev1.TinkerbellDatacenterConfig
	machineConfigs   map[string]*anywherev1.TinkerbellMachineConfig
	templateConfigs  map[string]*anywherev1.TinkerbellTemplateConfig
}

type TinkerbellFiller func(config TinkerbellConfig) error

func AutoFillTinkerbellProvider(filename string, fillers ...TinkerbellFiller) ([]byte, error) {
	tinkerbellDatacenterConfig, err := anywherev1.GetTinkerbellDatacenterConfig(filename)
	if err != nil {
		return nil, fmt.Errorf("unable to get tinkerbell datacenter config from file: %v", err)
	}

	tinkerbellMachineConfigs, err := anywherev1.GetTinkerbellMachineConfigs(filename)
	if err != nil {
		return nil, fmt.Errorf("unable to get tinkerbell machine config from file: %v", err)
	}

	tinkerbellTemplateConfigs, err := anywherev1.GetTinkerbellTemplateConfig(filename)
	if err != nil {
		return nil, fmt.Errorf("unable to get tinkerbell template configs from file: %v", err)
	}

	clusterConfig, err := anywherev1.GetClusterConfig(filename)
	if err != nil {
		return nil, fmt.Errorf("unable to get tinkerbell cluster config from file: %v", err)
	}

	config := TinkerbellConfig{
		clusterConfig:    clusterConfig,
		datacenterConfig: tinkerbellDatacenterConfig,
		machineConfigs:   tinkerbellMachineConfigs,
		templateConfigs:  tinkerbellTemplateConfigs,
	}

	for _, f := range fillers {
		err := f(config)
		if err != nil {
			return nil, fmt.Errorf("failed to apply tinkerbell config filler: %v", err)
		}
	}

	resources := make([]interface{}, 0, len(config.machineConfigs)+len(config.templateConfigs)+1)
	resources = append(resources, config.datacenterConfig)

	for _, m := range config.machineConfigs {
		resources = append(resources, m)
	}

	for _, m := range config.templateConfigs {
		resources = append(resources, m)
	}

	yamlResources := make([][]byte, 0, len(resources))
	for _, r := range resources {
		yamlContent, err := yaml.Marshal(r)
		if err != nil {
			return nil, fmt.Errorf("marshalling tinkerbell resource: %v", err)
		}

		yamlResources = append(yamlResources, yamlContent)
	}

	return templater.AppendYamlResources(yamlResources...), nil
}

func WithTinkerbellServer(value string) TinkerbellFiller {
	return func(config TinkerbellConfig) error {
		config.datacenterConfig.Spec.TinkerbellIP = value
		return nil
	}
}

func WithTinkerbellOSImageURL(value string) TinkerbellFiller {
	return func(config TinkerbellConfig) error {
		config.datacenterConfig.Spec.OSImageURL = value
		return nil
	}
}

func WithStringFromEnvVarTinkerbell(envVar string, opt func(string) TinkerbellFiller) TinkerbellFiller {
	return opt(os.Getenv(envVar))
}

func WithOsFamilyForAllTinkerbellMachines(value anywherev1.OSFamily) TinkerbellFiller {
	return func(config TinkerbellConfig) error {
		for _, m := range config.machineConfigs {
			m.Spec.OSFamily = value
		}
		return nil
	}
}

func WithImageUrlForAllTinkerbellMachines(value string) TinkerbellFiller {
	return func(config TinkerbellConfig) error {
		for _, t := range config.templateConfigs {
			for _, task := range t.Spec.Template.Tasks {
				for _, action := range task.Actions {
					if action.Name == "stream-image" {
						action.Environment["IMG_URL"] = value
					}
				}
			}
		}
		return nil
	}
}

func WithSSHAuthorizedKeyForAllTinkerbellMachines(key string) TinkerbellFiller {
	return func(config TinkerbellConfig) error {
		for _, m := range config.machineConfigs {
			if len(m.Spec.Users) == 0 {
				m.Spec.Users = []anywherev1.UserConfiguration{{}}
			}

			m.Spec.Users[0].Name = "ec2-user"
			m.Spec.Users[0].SshAuthorizedKeys = []string{key}
		}
		return nil
	}
}

func WithHardwareSelectorLabels() TinkerbellFiller {
	return func(config TinkerbellConfig) error {
		clusterName := config.clusterConfig.Name
		cpName := providers.GetControlPlaneNodeName(clusterName)
		workerName := clusterName

		cpMachineConfig := config.machineConfigs[cpName]
		cpMachineConfig.Spec.HardwareSelector = map[string]string{HardwareLabelTypeKeyName: ControlPlane}
		config.machineConfigs[cpName] = cpMachineConfig

		workerMachineConfig := config.machineConfigs[workerName]
		workerMachineConfig.Spec.HardwareSelector = map[string]string{HardwareLabelTypeKeyName: Worker}
		config.machineConfigs[workerName] = workerMachineConfig

		return nil
	}
}

func WithTinkerbellEtcdMachineConfig() TinkerbellFiller {
	return func(config TinkerbellConfig) error {
		clusterName := config.clusterConfig.Name
		name := providers.GetEtcdNodeName(clusterName)

		_, ok := config.machineConfigs[name]
		if !ok {
			m := &anywherev1.TinkerbellMachineConfig{
				TypeMeta: metav1.TypeMeta{
					Kind:       anywherev1.TinkerbellMachineConfigKind,
					APIVersion: anywherev1.SchemeBuilder.GroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
				Spec: anywherev1.TinkerbellMachineConfigSpec{
					HardwareSelector: map[string]string{HardwareLabelTypeKeyName: ExternalEtcd},
					TemplateRef: anywherev1.Ref{
						Name: clusterName,
						Kind: anywherev1.TinkerbellTemplateConfigKind,
					},
				},
			}
			config.machineConfigs[name] = m
		}
		return nil
	}
}

func WithCustomTinkerbellMachineConfig(selector string) TinkerbellFiller {
	return func(config TinkerbellConfig) error {
		if _, ok := config.machineConfigs[selector]; !ok {
			m := &anywherev1.TinkerbellMachineConfig{
				TypeMeta: metav1.TypeMeta{
					Kind:       anywherev1.TinkerbellMachineConfigKind,
					APIVersion: anywherev1.SchemeBuilder.GroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: selector,
				},
				Spec: anywherev1.TinkerbellMachineConfigSpec{
					HardwareSelector: map[string]string{HardwareLabelTypeKeyName: selector},
				},
			}
			config.machineConfigs[selector] = m
		}
		return nil
	}
}

func WithCustomControlPlaneTemplateConfig(tinkerbellBootstrapIp, tinkerbellIp, disk string, osFamily anywherev1.OSFamily) TinkerbellFiller {
	return func(config TinkerbellConfig) error {
		versionBundle, err := cluster.GetVersionsBundleForVersion(version.Get(), config.clusterConfig.Spec.KubernetesVersion)
		if err != nil {
			return fmt.Errorf("creating control plane node template config: %v", err)
		}

		clusterName := config.clusterConfig.Name
		cpName := providers.GetControlPlaneNodeName(clusterName)

		cpMachineConfig := config.machineConfigs[cpName]
		cpTemplateConfig := v1alpha1.NewDefaultTinkerbellTemplateConfigCreate(cpName, *versionBundle, disk, config.datacenterConfig.Spec.OSImageURL, tinkerbellBootstrapIp, tinkerbellIp, osFamily)
		config.templateConfigs[cpTemplateConfig.Name] = cpTemplateConfig

		cpMachineConfig.Spec.TemplateRef = anywherev1.Ref{
			Name: cpName,
			Kind: anywherev1.TinkerbellTemplateConfigKind,
		}

		return nil
	}
}

func WithCustomWorkerTemplateConfig(tinkerbellBootstrapIp, tinkerbellIp, disk string, osFamily anywherev1.OSFamily) TinkerbellFiller {
	return func(config TinkerbellConfig) error {
		versionBundle, err := cluster.GetVersionsBundleForVersion(version.Get(), config.clusterConfig.Spec.KubernetesVersion)
		if err != nil {
			return fmt.Errorf("creating worker node template config: %v", err)
		}

		workerName := config.clusterConfig.Name
		workerMachineConfig := config.machineConfigs[workerName]
		workerTemplateConfig := v1alpha1.NewDefaultTinkerbellTemplateConfigCreate(workerName, *versionBundle, disk, config.datacenterConfig.Spec.OSImageURL, tinkerbellBootstrapIp, tinkerbellIp, osFamily)
		config.templateConfigs[workerTemplateConfig.Name] = workerTemplateConfig

		workerMachineConfig.Spec.TemplateRef = anywherev1.Ref{
			Name: workerName,
			Kind: anywherev1.TinkerbellTemplateConfigKind,
		}

		return nil
	}
}
