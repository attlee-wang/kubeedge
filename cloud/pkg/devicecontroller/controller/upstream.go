/*
Copyright 2019 The KubeEdge Authors.

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

package controller

import (
	"encoding/json"
	"strconv"

	"k8s.io/client-go/rest"
	"k8s.io/klog"

	beehiveContext "github.com/kubeedge/beehive/pkg/core/context"
	"github.com/kubeedge/beehive/pkg/core/model"
	"github.com/kubeedge/kubeedge/cloud/pkg/apis/devices/v1alpha1"
	"github.com/kubeedge/kubeedge/cloud/pkg/devicecontroller/config"
	"github.com/kubeedge/kubeedge/cloud/pkg/devicecontroller/constants"
	"github.com/kubeedge/kubeedge/cloud/pkg/devicecontroller/messagelayer"
	"github.com/kubeedge/kubeedge/cloud/pkg/devicecontroller/types"
	"github.com/kubeedge/kubeedge/cloud/pkg/devicecontroller/utils"
)

// DeviceStatus is structure to patch device status
type DeviceStatus struct {
	Status v1alpha1.DeviceStatus `json:"status"`
}

const (
	// MergePatchType is patch type
	MergePatchType = "application/merge-patch+json"
	// ResourceTypeDevices is plural of device resource in apiserver
	ResourceTypeDevices = "devices"
)

// UpstreamController subscribe messages from edge and sync to k8s api server
type UpstreamController struct {
	crdClient    *rest.RESTClient
	messageLayer messagelayer.MessageLayer
	// message channel
	deviceStatusChan chan model.Message

	// downstream controller to update device status in cache
	dc *DownstreamController
}

// Start UpstreamController
func (uc *UpstreamController) Start() error {
	klog.Info("Start upstream devicecontroller")

	uc.deviceStatusChan = make(chan model.Message, config.UpdateDeviceStatusBuffer)

	go uc.dispatchMessage()

	for i := 0; i < config.UpdateDeviceStatusWorkers; i++ {
		go uc.updateDeviceStatus()
	}

	return nil
}

func (uc *UpstreamController) dispatchMessage() {
	for {
		select {
		case <-beehiveContext.Done():
			klog.Info("Stop dispatchMessage")
			return
		default:
		}
		msg, err := uc.messageLayer.Receive()
		if err != nil {
			klog.Warningf("Receive message failed, %s", err)
			continue
		}

		klog.Infof("Dispatch message: %s", msg.GetID())

		resourceType, err := messagelayer.GetResourceType(msg.GetResource())
		if err != nil {
			klog.Warningf("Parse message: %s resource type with error: %s", msg.GetID(), err)
			continue
		}
		klog.Infof("Message: %s, resource type is: %s", msg.GetID(), resourceType)

		switch resourceType {
		case constants.ResourceTypeTwinEdgeUpdated:
			uc.deviceStatusChan <- msg
		default:
			klog.Warningf("Message: %s, with resource type: %s not intended for device controller", msg.GetID(), resourceType)
		}
	}
}

func (uc *UpstreamController) updateDeviceStatus() {
	for {
		select {
		case <-beehiveContext.Done():
			klog.Info("Stop updateDeviceStatus")
			return
		case msg := <-uc.deviceStatusChan:
			klog.Infof("Message: %s, operation is: %s, and resource is: %s", msg.GetID(), msg.GetOperation(), msg.GetResource())
			msgTwin, err := uc.unmarshalDeviceStatusMessage(msg)
			if err != nil {
				klog.Warningf("Unmarshall failed due to error %v", err)
				continue
			}
			deviceID, err := messagelayer.GetDeviceID(msg.GetResource())
			if err != nil {
				klog.Warning("Failed to get device id")
				continue
			}
			device, ok := uc.dc.deviceManager.Device.Load(deviceID)
			if !ok {
				klog.Warningf("Device %s does not exist in downstream controller", deviceID)
				continue
			}
			cacheDevice, ok := device.(*v1alpha1.Device)
			if !ok {
				klog.Warning("Failed to assert to CacheDevice type")
				continue
			}
			deviceStatus := &DeviceStatus{Status: cacheDevice.Status}
			for twinName, twin := range msgTwin.Twin {
				for i, cacheTwin := range deviceStatus.Status.Twins {
					if twinName == cacheTwin.PropertyName && twin.Actual != nil && twin.Actual.Value != nil {
						reported := v1alpha1.TwinProperty{}
						reported.Value = *twin.Actual.Value
						reported.Metadata = make(map[string]string)
						if twin.Actual.Metadata != nil {
							reported.Metadata["timestamp"] = strconv.FormatInt(twin.Actual.Metadata.Timestamp, 10)
						}
						if twin.Metadata != nil {
							reported.Metadata["type"] = twin.Metadata.Type
						}
						deviceStatus.Status.Twins[i].Reported = reported
						break
					}
				}
			}

			// Store the status in cache so that when update is received by informer, it is not processed by downstream controller
			cacheDevice.Status = deviceStatus.Status
			uc.dc.deviceManager.Device.Store(deviceID, cacheDevice)

			body, err := json.Marshal(deviceStatus)
			if err != nil {
				klog.Errorf("Failed to marshal device status %v", deviceStatus)
				continue
			}
			result := uc.crdClient.Patch(MergePatchType).Namespace(cacheDevice.Namespace).Resource(ResourceTypeDevices).Name(deviceID).Body(body).Do()
			if result.Error() != nil {
				klog.Errorf("Failed to patch device status %v of device %v in namespace %v", deviceStatus, deviceID, cacheDevice.Namespace)
				continue
			}
			klog.Infof("Message: %s process successfully", msg.GetID())
		}
	}
}

func (uc *UpstreamController) unmarshalDeviceStatusMessage(msg model.Message) (*types.DeviceTwinUpdate, error) {
	content := msg.GetContent()
	twinUpdate := &types.DeviceTwinUpdate{}
	var contentData []byte
	var err error
	contentData, ok := content.([]byte)
	if !ok {
		contentData, err = json.Marshal(content)
		if err != nil {
			return nil, err
		}
	}
	err = json.Unmarshal(contentData, twinUpdate)
	if err != nil {
		return nil, err
	}
	return twinUpdate, nil
}

// NewUpstreamController create UpstreamController from config
func NewUpstreamController(dc *DownstreamController) (*UpstreamController, error) {
	config, err := utils.KubeConfig()
	crdcli, err := utils.NewCRDClient(config)
	ml, err := messagelayer.NewMessageLayer()
	if err != nil {
		klog.Warningf("Create message layer failed with error: %s", err)
	}
	uc := &UpstreamController{
		crdClient:    crdcli,
		messageLayer: ml,
		dc:           dc,
	}
	return uc, nil
}
