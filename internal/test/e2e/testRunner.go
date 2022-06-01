package e2e

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	aws_ssm "github.com/aws/aws-sdk-go/service/ssm"
	"github.com/aws/eks-anywhere/internal/pkg/ec2"
	"github.com/aws/eks-anywhere/internal/pkg/ssm"
	"github.com/aws/eks-anywhere/internal/pkg/vsphere"
	"github.com/aws/eks-anywhere/pkg/logger"
	"github.com/aws/eks-anywhere/pkg/retrier"
	"gopkg.in/yaml.v2"
)

const (
	testRunnerVCUserEnvVar     string = "TEST_RUNNER_GOVC_USERNAME"
	testRunnerVCPasswordEnvVar string = "TEST_RUNNER_GOVC_PASSWORD"
	govcUsernameKey            string = "GOVC_USERNAME"
	govcPasswordKey            string = "GOVC_PASSWORD"
	govcURLKey                 string = "GOVC_URL"
	govcInsecure               string = "GOVC_INSECURE"
	ssmActivationCodeKey       string = "ssm_activation_code"
	ssmActivationIdKey         string = "ssm_activation_id"
	ssmActivationRegionKey     string = "ssm_activation_region"
)

type TestRunner interface {
	createInstance(instanceConf instanceRunConf) (string, error)
	tagInstance(instanceConf instanceRunConf, key, value string) error
	decommInstance(instanceRunConf) error
}

type TestRunnerType string

const (
	Ec2TestRunnerType     TestRunnerType = "ec2"
	VSphereTestRunnerType TestRunnerType = "vSphere"
)

func newTestRunner(runnerType TestRunnerType, config TestInfraConfig) TestRunner {
	if runnerType == VSphereTestRunnerType {
		return &config.VSphereTestRunner
	} else {
		return &config.Ec2TestRunner
	}
}

type TestInfraConfig struct {
	Ec2TestRunner     `yaml:"ec2,omitempty"`
	VSphereTestRunner `yaml:"vSphere,omitempty"`
}

func NewTestRunnerConfigFromFile(configFile string) (*TestInfraConfig, error) {
	file, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create test runner config from file: %v", err)
	}

	config := TestInfraConfig{}

	err = yaml.Unmarshal(file, &config)
	if err != nil {
		return nil, fmt.Errorf("failed to create test runner config from file: %v", err)
	}

	return &config, nil
}

type testRunner struct {
	InstanceID string
}

type Ec2TestRunner struct {
	testRunner
	AmiID    string `yaml:"amiId"`
	SubnetID string `yaml:"subnetId"`
}

type VSphereTestRunner struct {
	testRunner
	ActivationId string
	Url          string `yaml:"url"`
	Insecure     bool   `yaml:"insecure"`
	Library      string `yaml:"library"`
	Template     string `yaml:"template"`
	Datacenter   string `yaml:"datacenter"`
	Datastore    string `yaml:"datastore"`
	ResourcePool string `yaml:"resourcePool"`
	Network      string `yaml:"network"`
	Folder       string `yaml:"folder"`
}

func (v *VSphereTestRunner) setEnvironment() error {
	if vSphereUsername, ok := os.LookupEnv(testRunnerVCUserEnvVar); ok && len(vSphereUsername) > 0 {
		if err := os.Setenv(govcUsernameKey, vSphereUsername); err != nil {
			return fmt.Errorf("unable to set %s: %v", govcUsernameKey, err)
		}
	} else {
		return fmt.Errorf("missing environment variable: %s", testRunnerVCUserEnvVar)
	}

	if vSpherePassword, ok := os.LookupEnv(testRunnerVCPasswordEnvVar); ok && len(vSpherePassword) > 0 {
		if err := os.Setenv(govcPasswordKey, vSpherePassword); err != nil {
			return fmt.Errorf("unable to set %s: %v", govcPasswordKey, err)
		}
	} else {
		return fmt.Errorf("missing environment variable: %s", testRunnerVCPasswordEnvVar)
	}

	if err := os.Setenv(govcURLKey, v.Url); err != nil {
		return fmt.Errorf("unable to set %s: %v", govcURLKey, err)
	}

	if err := os.Setenv(govcInsecure, strconv.FormatBool(v.Insecure)); err != nil {
		return fmt.Errorf("unable to set %s: %v", govcURLKey, err)
	}

	return nil
}

func (v *VSphereTestRunner) createInstance(c instanceRunConf) (string, error) {
	name := getTestRunnerName(c.jobId)
	logger.V(1).Info("Creating vSphere Test Runner instance", "name", name)

	err := v.setEnvironment()
	if err != nil {
		return "", fmt.Errorf("unable to create vSphere test runner instance: %v", err)
	}

	ssmActivationInfo, err := ssm.CreateActivation(c.session, name, c.instanceProfileName)
	if err != nil {
		return "", fmt.Errorf("unable to create ssm activation: %v", err)
	}

	// TODO: import ova template from url if not exist

	opts := vsphere.OVFDeployOptions{
		Name:             name,
		PowerOn:          true,
		DiskProvisioning: "thin",
		WaitForIP:        true,
		InjectOvfEnv:     true,
		NetworkMappings:  []vsphere.NetworkMapping{{Name: v.Network, Network: v.Network}},
		PropertyMapping: []vsphere.OVFProperty{
			{Key: ssmActivationCodeKey, Value: ssmActivationInfo.ActivationCode},
			{Key: ssmActivationIdKey, Value: ssmActivationInfo.ActivationID},
			{Key: ssmActivationRegionKey, Value: *c.session.Config.Region},
		},
	}

	// deploy template
	if err := vsphere.DeployTemplate(v.Library, v.Template, name, v.Folder, v.Datacenter, v.Datastore, v.ResourcePool, opts); err != nil {
		return "", err
	}

	var ssmInstance *aws_ssm.InstanceInformation
	err = retrier.Retry(10, 5*time.Second, func() error {
		ssmInstance, err = ssm.GetInstanceByActivationId(c.session, ssmActivationInfo.ActivationID)
		if err != nil {
			return fmt.Errorf("failed to get ssm instance info post ovf deployment: %v", err)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("waiting for ssm instance to activate %s : %v", name, err)
	}

	v.InstanceID = *ssmInstance.InstanceId
	v.ActivationId = ssmActivationInfo.ActivationID

	return *ssmInstance.InstanceId, nil
}

func (e *Ec2TestRunner) createInstance(c instanceRunConf) (string, error) {
	name := getTestRunnerName(c.jobId)
	logger.V(1).Info("Creating ec2 Test Runner instance", "name", name)
	instanceId, err := ec2.CreateInstance(c.session, e.AmiID, key, tag, c.instanceProfileName, e.SubnetID, name)
	if err != nil {
		return "", fmt.Errorf("creating instance for e2e tests: %v", err)
	}
	logger.V(1).Info("Instance created", "instance-id", instanceId)
	e.InstanceID = instanceId
	return instanceId, nil
}

func (v *VSphereTestRunner) tagInstance(c instanceRunConf, key, value string) error {
	vmName := getTestRunnerName(c.jobId)
	vmPath := fmt.Sprintf("/%s/vm/%s/%s", v.Datacenter, v.Folder, vmName)
	tag := fmt.Sprintf("%s:%s", key, value)
	if err := vsphere.TagVirtualMachine(vmPath, tag); err != nil {
		return fmt.Errorf("failed to tag vSphere test runner: %v", err)
	}
	return nil
}

func (e *Ec2TestRunner) tagInstance(c instanceRunConf, key, value string) error {
	err := ec2.TagInstance(c.session, c.instanceId, key, value)
	if err != nil {
		return fmt.Errorf("failed to tag Ec2 test runner: %v", err)
	}
	return nil
}

func (v *VSphereTestRunner) decommInstance(c instanceRunConf) error {
	_, deregisterError := ssm.DeregisterInstance(c.session, v.InstanceID)
	_, deactivateError := ssm.DeleteActivation(c.session, v.ActivationId)
	deleteError := vsphereRmVms(context.Background(), getTestRunnerName(c.jobId))

	if deregisterError != nil {
		return fmt.Errorf("failed to decommission vsphere test runner ssm instance: %v", deregisterError)
	}

	if deactivateError != nil {
		return fmt.Errorf("failed to decommission vsphere test runner ssm instance: %v", deactivateError)
	}

	if deleteError != nil {
		return fmt.Errorf("failed to decommission vsphere test runner ssm instance: %v", deleteError)
	}

	return nil
}

func (v *Ec2TestRunner) decommInstance(c instanceRunConf) error {
	return nil
}

func getTestRunnerName(jobId string) string {
	return fmt.Sprintf("eksa-e2e-%s", jobId)
}
