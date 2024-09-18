/*
2024 NVIDIA CORPORATION & AFFILIATES
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

package host

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Mellanox/nic-configuration-operator/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/Mellanox/nic-configuration-operator/pkg/consts"
)

// HostManager contains logic for managing NIC devices on the host
type HostManager interface {
	// DiscoverNicDevices discovers Nvidia NIC devices on the host and returns back a map of serial numbers to device statuses
	DiscoverNicDevices() (map[string]v1alpha1.NicDeviceStatus, error)
	// ValidateDeviceNvSpec will validate device's non-volatile spec against already applied configuration on the host
	// returns bool - nv config update required
	// returns bool - reboot required
	// returns error - there are errors in device's spec
	ValidateDeviceNvSpec(ctx context.Context, device *v1alpha1.NicDevice) (bool, bool, error)
	// ApplyDeviceNvSpec calculates device's missing nv spec configuration and applies it to the device on the host
	// returns bool - reboot required
	// returns error - there were errors while applying nv configuration
	ApplyDeviceNvSpec(ctx context.Context, device *v1alpha1.NicDevice) (bool, error)
	// ApplyDeviceRuntimeSpec calculates device's missing runtime spec configuration and applies it to the device on the host
	// returns error - there were errors while applying nv configuration
	ApplyDeviceRuntimeSpec(device *v1alpha1.NicDevice) error
}

type hostManager struct {
	nodeName         string
	hostUtils        HostUtils
	configValidation configValidation
}

// DiscoverNicDevices uses host utils to discover Nvidia NIC devices on the host and returns back a map of serial numbers to device statuses
func (h hostManager) DiscoverNicDevices() (map[string]v1alpha1.NicDeviceStatus, error) {
	log.Log.Info("HostManager.DiscoverNicDevices()")

	pciDevices, err := h.hostUtils.GetPCIDevices()
	if err != nil {
		log.Log.Error(err, "Failed to get PCI devices")
		return nil, err
	}

	// Map of Serial Number to nic device
	devices := make(map[string]v1alpha1.NicDeviceStatus)

	for _, device := range pciDevices {
		if device.Vendor.ID != consts.MellanoxVendor {
			continue
		}

		devClass, err := strconv.ParseInt(device.Class.ID, 16, 64)
		if err != nil {
			log.Log.Error(err, "DiscoverSriovDevices(): unable to parse device class, skipping",
				"device", device)
			continue
		}
		if devClass != consts.NetClass {
			log.Log.V(2).Info("Device is not a network device, skipping", "address", device)
			continue
		}

		if h.hostUtils.IsSriovVF(device.Address) {
			log.Log.V(2).Info("Device is an SRIOV VF, skipping", "address", device.Address)
			continue
		}

		log.Log.Info("Found Mellanox device", "address", device.Address, "type", device.Product.Name)

		partNumber, serialNumber, err := h.hostUtils.GetPartAndSerialNumber(device.Address)
		if err != nil {
			log.Log.Error(err, "Failed to get device's part and serial numbers", "address", device.Address)
			return nil, err
		}

		// Devices with the same serial number are ports of the same NIC, so grouping them
		deviceStatus, ok := devices[serialNumber]

		if !ok {
			firmwareVersion, psid, err := h.hostUtils.GetFirmwareVersionAndPSID(device.Address)
			if err != nil {
				log.Log.Error(err, "Failed to get device's firmware and PSID", "address", device.Address)
				return nil, err
			}

			deviceStatus = v1alpha1.NicDeviceStatus{
				Type:            device.Product.ID,
				SerialNumber:    serialNumber,
				PartNumber:      partNumber,
				PSID:            psid,
				FirmwareVersion: firmwareVersion,
				Ports:           []v1alpha1.NicDevicePortSpec{},
			}

			devices[serialNumber] = deviceStatus
		}

		networkInterface := h.hostUtils.GetInterfaceName(device.Address)
		rdmaInterface := h.hostUtils.GetRDMADeviceName(device.Address)

		deviceStatus.Ports = append(deviceStatus.Ports, v1alpha1.NicDevicePortSpec{
			PCI:              device.Address,
			NetworkInterface: networkInterface,
			RdmaInterface:    rdmaInterface,
		})

		deviceStatus.Node = h.nodeName
		devices[deviceStatus.SerialNumber] = deviceStatus
	}

	return devices, nil
}

// ValidateDeviceNvSpec will validate device's non-volatile spec against already applied configuration on the host
// returns bool - nv config update required
// returns bool - reboot required
// returns error - there are errors in device's spec
// if fully matches in current and next config, returns false, false
// if fully matched next but not current, returns false, true
// if not fully matched next boot, returns true, true
func (h hostManager) ValidateDeviceNvSpec(ctx context.Context, device *v1alpha1.NicDevice) (bool, bool, error) {
	log.Log.Info("hostManager.ValidateDeviceNvSpec", "device", device.Name)

	nvConfig, err := h.hostUtils.QueryNvConfig(ctx, device.Status.Ports[0].PCI)
	if err != nil {
		log.Log.Error(err, "failed to query nv config", "device", device.Name)
		return false, false, err
	}

	if device.Spec.Configuration.ResetToDefault {
		return h.configValidation.ValidateResetToDefault(nvConfig)
	}

	desiredConfig, err := h.configValidation.ConstructNvParamMapFromTemplate(device, nvConfig.DefaultConfig)
	if err != nil {
		log.Log.Error(err, "failed to calculate desired nvconfig parameters", "device", device.Name)
		return false, false, err
	}

	configUpdateNeeded := false
	rebootNeeded := false

	// If ADVANCED_PCI_SETTINGS are enabled in current config, unknown parameters are treated as spec error
	advancedPciSettingsEnabled := h.configValidation.AdvancedPCISettingsEnabled(nvConfig.CurrentConfig)

	for parameter, desiredValue := range desiredConfig {
		currentValue, foundInCurrent := nvConfig.CurrentConfig[parameter]
		nextValue, foundInNextBoot := nvConfig.NextBootConfig[parameter]
		if advancedPciSettingsEnabled && !foundInCurrent {
			err = types.IncorrectSpecError(fmt.Sprintf("Parameter %s unsupported for device %s", parameter, device.Name))
			log.Log.Error(err, "can't set nv config parameter for device")
			return false, false, err
		}

		if foundInNextBoot && nextValue == desiredValue {
			if !foundInCurrent || currentValue != desiredValue {
				rebootNeeded = true
			}
		} else {
			configUpdateNeeded = true
			rebootNeeded = true
		}
	}

	return configUpdateNeeded, rebootNeeded, nil
}

// ApplyDeviceNvSpec calculates device's missing nv spec configuration and applies it to the device on the host
// returns bool - reboot required
// returns error - there were errors while applying nv configuration
func (h hostManager) ApplyDeviceNvSpec(ctx context.Context, device *v1alpha1.NicDevice) (bool, error) {
	log.Log.Info("hostManager.ApplyDeviceNvSpec", "device", device.Name)

	pciAddr := device.Status.Ports[0].PCI

	if device.Spec.Configuration.ResetToDefault == true {
		log.Log.Info("resetting nv config to default", "device", device.Name) // todo
		err := h.hostUtils.ResetNvConfig(pciAddr)
		if err != nil {
			log.Log.Error(err, "Failed to reset nv config", "device", device.Name)
			return false, err
		}

		err = h.hostUtils.SetNvConfigParameter(pciAddr, consts.AdvancedPCISettingsParam, consts.NvParamTrue)
		if err != nil {
			log.Log.Error(err, "Failed to apply nv config parameter", "device", device.Name, "param", consts.AdvancedPCISettingsParam, "value", consts.NvParamTrue)
			return false, err
		}

		return true, err
	}

	nvConfig, err := h.hostUtils.QueryNvConfig(ctx, device.Status.Ports[0].PCI)
	if err != nil {
		log.Log.Error(err, "failed to query nv config", "device", device.Name)
		return false, err
	}

	if !h.configValidation.AdvancedPCISettingsEnabled(nvConfig.CurrentConfig) {
		log.Log.Info("AdvancedPciSettings not enabled, fw reset required", "device", device.Name) // todo
		err = h.hostUtils.SetNvConfigParameter(pciAddr, consts.AdvancedPCISettingsParam, consts.NvParamTrue)
		if err != nil {
			log.Log.Error(err, "Failed to apply nv config parameter", "device", device.Name, "param", consts.AdvancedPCISettingsParam, "value", consts.NvParamTrue)
			return false, err
		}

		err = h.hostUtils.ResetNicFirmware(ctx, pciAddr)
		if err != nil {
			log.Log.Error(err, "Failed to reset NIC firmware", "device", device.Name)
			return false, err
		}

		// Query nv config again, additional options could become available
		nvConfig, err = h.hostUtils.QueryNvConfig(ctx, device.Status.Ports[0].PCI)
		if err != nil {
			log.Log.Error(err, "failed to query nv config", "device", device.Name)
			return false, err
		}
	}

	desiredConfig, err := h.configValidation.ConstructNvParamMapFromTemplate(device, nvConfig.DefaultConfig)
	if err != nil {
		log.Log.Error(err, "failed to calculate desired nvconfig parameters", "device", device.Name)
		return false, err
	}

	paramsToApply := map[string]string{}

	for param, value := range desiredConfig {
		nextVal, found := nvConfig.NextBootConfig[param]
		if !found {
			err = types.IncorrectSpecError(fmt.Sprintf("Parameter %s unsupported for device %s", param, device.Name))
			log.Log.Error(err, "can't set nv config parameter for device")
			return false, err
		}

		if nextVal != value {
			paramsToApply[param] = value
		}
	}

	log.Log.V(2).Info("applying nv config to device", "device", device.Name, "config", paramsToApply)

	for param, value := range paramsToApply {
		err = h.hostUtils.SetNvConfigParameter(pciAddr, param, value)
		if err != nil {
			log.Log.Error(err, "Failed to apply nv config parameter", "device", device.Name, "param", param, "value", value)
			return false, err
		}
	}

	log.Log.V(2).Info("nv config succesful applied to device", "device", device.Name)

	return true, nil
}

// ApplyDeviceRuntimeSpec calculates device's missing runtime spec configuration and applies it to the device on the host
// returns error - there were errors while applying nv configuration
func (h hostManager) ApplyDeviceRuntimeSpec(device *v1alpha1.NicDevice) error {
	log.Log.Info("hostManager.ApplyDeviceRuntimeSpec", "device", device.Name)

	alreadyApplied, err := h.configValidation.RuntimeConfigApplied(device)
	if err != nil {
		log.Log.Error(err, "failed to verify runtime configuration", "device", device)
	}

	if alreadyApplied {
		log.Log.V(2).Info("runtime config already applied", "device", device)
		return nil
	}

	desiredMaxReadReqSize, desiredTrust, desiredPfc := h.configValidation.CalculateDesiredRuntimeConfig(device)

	ports := device.Status.Ports

	if desiredMaxReadReqSize != 0 {
		err = h.hostUtils.SetMaxReadRequestSize(ports[0].PCI, desiredMaxReadReqSize)
		if err != nil {
			log.Log.Error(err, "failed to apply runtime configuration", "device", device)
			return err
		}
		if len(ports) == 2 {
			err = h.hostUtils.SetMaxReadRequestSize(ports[1].PCI, desiredMaxReadReqSize)
			if err != nil {
				log.Log.Error(err, "failed to apply runtime configuration", "device", device)
				return err
			}
		}
	}

	err = h.hostUtils.SetTrustAndPFC(ports[0].NetworkInterface, desiredTrust, desiredPfc)
	if err != nil {
		log.Log.Error(err, "failed to apply runtime configuration", "device", device)
		return err
	}
	if len(ports) == 2 {
		err = h.hostUtils.SetTrustAndPFC(ports[1].NetworkInterface, desiredTrust, desiredPfc)
		if err != nil {
			log.Log.Error(err, "failed to apply runtime configuration", "device", device)
			return err
		}
	}

	return nil
}

func NewHostManager(nodeName string, hostUtils HostUtils) HostManager {
	return hostManager{nodeName: nodeName, hostUtils: hostUtils, configValidation: newConfigValidation(hostUtils)}
}
