/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubeletconfig

import (
	"fmt"
	"path/filepath"
	"time"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/kubelet/apis/kubeletconfig"
	"k8s.io/kubernetes/pkg/kubelet/apis/kubeletconfig/validation"

	"k8s.io/kubernetes/pkg/kubelet/kubeletconfig/checkpoint"
	"k8s.io/kubernetes/pkg/kubelet/kubeletconfig/checkpoint/store"
	"k8s.io/kubernetes/pkg/kubelet/kubeletconfig/configfiles"
	"k8s.io/kubernetes/pkg/kubelet/kubeletconfig/status"
	utillog "k8s.io/kubernetes/pkg/kubelet/kubeletconfig/util/log"
	utilpanic "k8s.io/kubernetes/pkg/kubelet/kubeletconfig/util/panic"
	utilfs "k8s.io/kubernetes/pkg/util/filesystem"
)

const (
	checkpointsDir = "checkpoints"
)

// Controller is the controller which, among other things:
// - loads configuration from disk
// - checkpoints configuration to disk
// - downloads new configuration from the API server
// - validates configuration
// - tracks the last-known-good configuration, and rolls-back to last-known-good when necessary
// For more information, see the proposal: https://github.com/kubernetes/community/blob/master/contributors/design-proposals/node/dynamic-kubelet-configuration.md
type Controller struct {
	// dynamicConfig, if true, indicates that we should sync config from the API server
	dynamicConfig bool

	// defaultConfig is the configuration to use if no initConfig is provided
	defaultConfig *kubeletconfig.KubeletConfiguration

	// fileLoader is for loading the Kubelet's local config files from disk
	fileLoader configfiles.Loader

	// pendingConfigSource; write to this channel to indicate that the config source needs to be synced from the API server
	pendingConfigSource chan bool

	// configOK manages the ConfigOK condition that is reported in Node.Status.Conditions
	configOK status.ConfigOKCondition

	// informer is the informer that watches the Node object
	informer cache.SharedInformer

	// checkpointStore persists config source checkpoints to a storage layer
	checkpointStore store.Store
}

// NewController constructs a new Controller object and returns it. Directory paths must be absolute.
// If the `kubeletConfigFile` is an empty string, skips trying to load the kubelet config file.
// If the `dynamicConfigDir` is an empty string, skips trying to load checkpoints or download new config,
// but will still sync the ConfigOK condition if you call StartSync with a non-nil client.
func NewController(defaultConfig *kubeletconfig.KubeletConfiguration,
	kubeletConfigFile string,
	dynamicConfigDir string) (*Controller, error) {
	var err error

	fs := utilfs.DefaultFs{}

	var fileLoader configfiles.Loader
	if len(kubeletConfigFile) > 0 {
		fileLoader, err = configfiles.NewFsLoader(fs, kubeletConfigFile)
		if err != nil {
			return nil, err
		}
	}
	dynamicConfig := false
	if len(dynamicConfigDir) > 0 {
		dynamicConfig = true
	}

	return &Controller{
		dynamicConfig: dynamicConfig,
		defaultConfig: defaultConfig,
		// channels must have capacity at least 1, since we signal with non-blocking writes
		pendingConfigSource: make(chan bool, 1),
		configOK:            status.NewConfigOKCondition(),
		checkpointStore:     store.NewFsStore(fs, filepath.Join(dynamicConfigDir, checkpointsDir)),
		fileLoader:          fileLoader,
	}, nil
}

// Bootstrap attempts to return a valid KubeletConfiguration based on the configuration of the Controller,
// or returns an error if no valid configuration could be produced. Bootstrap should be called synchronously before StartSync.
func (cc *Controller) Bootstrap() (*kubeletconfig.KubeletConfiguration, error) {
	utillog.Infof("starting controller")

	// Load and validate the local config (defaults + flags, file)
	local, err := cc.loadLocalConfig()
	if err != nil {
		return nil, err
	} // Assert: the default and file configs are both valid

	// if dynamic config is disabled, we just stop here
	if !cc.dynamicConfig {
		// NOTE(mtaufen): We still need to update the status.
		// We expect to be able to disable dynamic config but still get a status update about the config.
		// This is because the feature gate covers dynamic config AND config status reporting, while the
		// --dynamic-config-dir flag just covers dynamic config.
		cc.configOK.Set(status.NotDynamicLocalMessage, status.NotDynamicLocalReason, apiv1.ConditionTrue)
		return local, nil
	} // Assert: dynamic config is enabled

	// ensure the filesystem is initialized
	if err := cc.initializeDynamicConfigDir(); err != nil {
		return nil, err
	}

	assigned, curSource, reason, err := cc.loadAssignedConfig(local)
	if err == nil {
		// set the status to indicate we will use the assigned config
		if curSource != nil {
			cc.configOK.Set(fmt.Sprintf(status.CurRemoteMessageFmt, curSource.UID()), reason, apiv1.ConditionTrue)
		} else {
			cc.configOK.Set(status.CurLocalMessage, reason, apiv1.ConditionTrue)
		}

		// when the trial period is over, the assigned config becomes the last-known-good
		if trial, err := cc.inTrial(assigned.ConfigTrialDuration.Duration); err != nil {
			utillog.Errorf("failed to check trial period for assigned config, error: %v", err)
		} else if !trial {
			utillog.Infof("assigned config passed trial period, will set as last-known-good")
			if err := cc.graduateAssignedToLastKnownGood(); err != nil {
				utillog.Errorf("failed to set last-known-good to assigned config, error: %v", err)
			}
		}

		return assigned, nil
	} // Assert: the assigned config failed to load, parse, or validate

	// TODO(mtaufen): consider re-attempting download when a load/verify/parse/validate
	// error happens outside trial period, we already made it past the trial so it's probably filesystem corruption
	// or something else scary (unless someone is using a 0-length trial period)
	// load from checkpoint

	// log the reason and error details for the failure to load the assigned config
	utillog.Errorf(fmt.Sprintf("%s, error: %v", reason, err))

	// load the last-known-good config
	lkg, lkgSource, err := cc.loadLastKnownGoodConfig(local)
	if err != nil {
		return nil, err
	}

	// set the status to indicate that we had to roll back to the lkg for the reason reported when we tried to load the assigned config
	if lkgSource != nil {
		cc.configOK.Set(fmt.Sprintf(status.LkgRemoteMessageFmt, lkgSource.UID()), reason, apiv1.ConditionFalse)
	} else {
		cc.configOK.Set(status.LkgLocalMessage, reason, apiv1.ConditionFalse)
	}

	// return the last-known-good config
	return lkg, nil
}

// StartSync launches the controller's sync loops if `client` is non-nil and `nodeName` is non-empty.
// It will always start the Node condition reporting loop, and will also start the dynamic conifg sync loops
// if dynamic config is enabled on the controller. If `nodeName` is empty but `client` is non-nil, an error is logged.
func (cc *Controller) StartSync(client clientset.Interface, eventClient v1core.EventsGetter, nodeName string) {
	if client == nil {
		utillog.Infof("nil client, will not start sync loops")
		return
	} else if len(nodeName) == 0 {
		utillog.Errorf("cannot start sync loops with empty nodeName")
		return
	}

	// start the ConfigOK condition sync loop
	go utilpanic.HandlePanic(func() {
		utillog.Infof("starting ConfigOK condition sync loop")
		wait.JitterUntil(func() {
			cc.configOK.Sync(client, nodeName)
		}, 10*time.Second, 0.2, true, wait.NeverStop)
	})()

	// only sync to new, remotely provided configurations if dynamic config was enabled
	if cc.dynamicConfig {
		cc.informer = newSharedNodeInformer(client, nodeName,
			cc.onAddNodeEvent, cc.onUpdateNodeEvent, cc.onDeleteNodeEvent)
		// start the informer loop
		// Rather than use utilruntime.HandleCrash, which doesn't actually crash in the Kubelet,
		// we use HandlePanic to manually call the panic handlers and then crash.
		// We have a better chance of recovering normal operation if we just restart the Kubelet in the event
		// of a Go runtime error.
		go utilpanic.HandlePanic(func() {
			utillog.Infof("starting Node informer sync loop")
			cc.informer.Run(wait.NeverStop)
		})()

		// start the config source sync loop
		go utilpanic.HandlePanic(func() {
			utillog.Infof("starting config source sync loop")
			wait.JitterUntil(func() {
				cc.syncConfigSource(client, eventClient, nodeName)
			}, 10*time.Second, 0.2, true, wait.NeverStop)
		})()
	} else {
		utillog.Infof("dynamic config not enabled, will not sync to remote config")
	}
}

// loadLocalConfig returns the local config: either the defaults provided to the controller or
// a local config file, if the Kubelet is configured to use the local file
func (cc *Controller) loadLocalConfig() (*kubeletconfig.KubeletConfiguration, error) {
	// ALWAYS validate the local configs. This makes incorrectly provisioned nodes an error.
	// These must be valid because they are the default last-known-good configs.
	utillog.Infof("validating combination of defaults and flags")
	if err := validation.ValidateKubeletConfiguration(cc.defaultConfig); err != nil {
		return nil, fmt.Errorf("combination of defaults and flags failed validation, error: %v", err)
	}
	// only attempt to load and validate the Kubelet config file if the user provided a path
	if cc.fileLoader != nil {
		utillog.Infof("loading Kubelet config file")
		kc, err := cc.fileLoader.Load()
		if err != nil {
			return nil, err
		}
		// validate the Kubelet config file config
		utillog.Infof("validating Kubelet config file")
		if err := validation.ValidateKubeletConfiguration(kc); err != nil {
			return nil, fmt.Errorf("failed to validate the Kubelet config file, error: %v", err)
		}
		return kc, nil
	}
	// if no Kubelet config file config, just return the default
	return cc.defaultConfig, nil
}

// loadAssignedConfig loads the Kubelet's currently assigned config,
// based on the setting in the local checkpoint store.
// It returns the loaded configuration, the checkpoint store's config source record,
// a clean success or failure reason that can be reported in the status, and any error that occurs.
// If the local config should be used, it will be returned. You should validate local before passing it to this function.
func (cc *Controller) loadAssignedConfig(local *kubeletconfig.KubeletConfiguration) (*kubeletconfig.KubeletConfiguration, checkpoint.RemoteConfigSource, string, error) {
	src, err := cc.checkpointStore.Current()
	if err != nil {
		return nil, nil, fmt.Sprintf(status.CurFailLoadReasonFmt, "unknown"), err
	}
	// nil source is the signal to use the local config
	if src == nil {
		return local, src, status.CurLocalOkayReason, nil
	}
	curUID := src.UID()
	// load from checkpoint
	checkpoint, err := cc.checkpointStore.Load(curUID)
	if err != nil {
		return nil, src, fmt.Sprintf(status.CurFailLoadReasonFmt, curUID), err
	}
	cur, err := checkpoint.Parse()
	if err != nil {
		return nil, src, fmt.Sprintf(status.CurFailParseReasonFmt, curUID), err
	}
	if err := validation.ValidateKubeletConfiguration(cur); err != nil {
		return nil, src, fmt.Sprintf(status.CurFailValidateReasonFmt, curUID), err
	}
	return cur, src, status.CurRemoteOkayReason, nil
}

// loadLastKnownGoodConfig loads the Kubelet's last-known-good config,
// based on the setting in the local checkpoint store.
// It returns the loaded configuration, the checkpoint store's config source record,
// and any error that occurs.
// If the local config should be used, it will be returned. You should validate local before passing it to this function.
func (cc *Controller) loadLastKnownGoodConfig(local *kubeletconfig.KubeletConfiguration) (*kubeletconfig.KubeletConfiguration, checkpoint.RemoteConfigSource, error) {
	src, err := cc.checkpointStore.LastKnownGood()
	if err != nil {
		return nil, nil, fmt.Errorf("unable to determine last-known-good config, error: %v", err)
	}
	// nil source is the signal to use the local config
	if src == nil {
		return local, src, nil
	}
	lkgUID := src.UID()
	// load from checkpoint
	checkpoint, err := cc.checkpointStore.Load(lkgUID)
	if err != nil {
		return nil, src, fmt.Errorf("%s, error: %v", fmt.Sprintf(status.LkgFailLoadReasonFmt, lkgUID), err)
	}
	lkg, err := checkpoint.Parse()
	if err != nil {
		return nil, src, fmt.Errorf("%s, error: %v", fmt.Sprintf(status.LkgFailParseReasonFmt, lkgUID), err)
	}
	if err := validation.ValidateKubeletConfiguration(lkg); err != nil {
		return nil, src, fmt.Errorf("%s, error: %v", fmt.Sprintf(status.LkgFailValidateReasonFmt, lkgUID), err)
	}
	return lkg, src, nil
}

// initializeDynamicConfigDir makes sure that the storage layers for various controller components are set up correctly
func (cc *Controller) initializeDynamicConfigDir() error {
	utillog.Infof("ensuring filesystem is set up correctly")
	// initializeDynamicConfigDir local checkpoint storage location
	return cc.checkpointStore.Initialize()
}

// inTrial returns true if the time elapsed since the last modification of the current config does not exceed `trialDur`, false otherwise
func (cc *Controller) inTrial(trialDur time.Duration) (bool, error) {
	now := time.Now()
	t, err := cc.checkpointStore.CurrentModified()
	if err != nil {
		return false, err
	}
	if now.Sub(t) <= trialDur {
		return true, nil
	}
	return false, nil
}

// graduateAssignedToLastKnownGood sets the last-known-good UID on the checkpointStore
// to the same value as the current UID maintained by the checkpointStore
func (cc *Controller) graduateAssignedToLastKnownGood() error {
	curUID, err := cc.checkpointStore.Current()
	if err != nil {
		return err
	}
	err = cc.checkpointStore.SetLastKnownGood(curUID)
	if err != nil {
		return err
	}
	return nil
}
