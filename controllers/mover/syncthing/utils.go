package syncthing

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/backube/volsync/api/v1alpha1"
)

// UpdateDevices Updates the Syncthing's connected devices with the provided peerList.
func (st *Syncthing) UpdateDevices(peerList []v1alpha1.SyncthingPeer) {
	st.logger.V(4).Info("Updating devices", "peerlist", peerList)

	// update syncthing config based on the provided peerlist
	newDevices := []SyncthingDevice{}

	// add myself and introduced devices to the device list
	for _, device := range st.Config.Devices {
		if device.DeviceID == st.SystemStatus.MyID || device.IntroducedBy != "" {
			newDevices = append(newDevices, device)
		}
	}

	// Add the devices from the peerList to the device list
	for _, device := range peerList {
		stDeviceToAdd := SyncthingDevice{
			DeviceID:   device.ID,
			Addresses:  []string{device.Address},
			Introducer: device.Introducer,
		}
		st.logger.V(4).Info("Adding device: %+v\n", stDeviceToAdd)
		newDevices = append(newDevices, stDeviceToAdd)
	}

	st.Config.Devices = newDevices
	st.logger.V(4).Info("Updated devices", "devices", st.Config.Devices)

	// update folders with the new devices
	st.updateFolders()
}

// updateFolders Updates all of Syncthing's folders to be shared with all configured devices.
func (st *Syncthing) updateFolders() {
	// share the current folder(s) with the new devices
	var newFolders = []SyncthingFolder{}
	for _, folder := range st.Config.Folders {
		// copy folder & reset
		newFolder := folder
		newFolder.Devices = []FolderDeviceConfiguration{}

		for _, device := range st.Config.Devices {
			newFolder.Devices = append(newFolder.Devices, FolderDeviceConfiguration{
				DeviceID:     device.DeviceID,
				IntroducedBy: device.IntroducedBy,
			})
		}
		newFolders = append(newFolders, newFolder)
	}
	st.Config.Folders = newFolders
}

// NeedsReconfigure Determines whether the given nodeList differs from Syncthing's internal devices.
func (st *Syncthing) NeedsReconfigure(nodeList []v1alpha1.SyncthingPeer) bool {
	// check if the syncthing nodelist diverges from the current syncthing devices
	var newDevices map[string]v1alpha1.SyncthingPeer = map[string]v1alpha1.SyncthingPeer{
		// initialize the map with the self node
		st.SystemStatus.MyID: {
			ID:      st.SystemStatus.MyID,
			Address: "",
		},
	}

	// add all of the other devices in the provided nodeList
	for _, device := range nodeList {
		// avoid self
		if device.ID == st.SystemStatus.MyID {
			continue
		}
		newDevices[device.ID] = device
	}

	// create a map for current devices
	var currentDevs map[string]v1alpha1.SyncthingPeer = map[string]v1alpha1.SyncthingPeer{
		// initialize the map with the self node
		st.SystemStatus.MyID: {
			ID:      st.SystemStatus.MyID,
			Address: "",
		},
	}
	// add the rest of devices to the map
	for _, device := range st.Config.Devices {
		// ignore self and introduced devices
		if device.DeviceID == st.SystemStatus.MyID || device.IntroducedBy != "" {
			continue
		}

		currentDevs[device.DeviceID] = v1alpha1.SyncthingPeer{
			ID:      device.DeviceID,
			Address: device.Addresses[0],
		}
	}

	// check if the syncthing nodelist diverges from the current syncthing devices
	for _, device := range newDevices {
		if _, ok := currentDevs[device.ID]; !ok {
			return true
		}
	}
	for _, device := range currentDevs {
		if _, ok := newDevices[device.ID]; !ok {
			return true
		}
	}
	return false
}

// collectIntroduced Returns a map of DeviceID -> Device for devices which have been introduced to us by another node.
func (st *Syncthing) collectIntroduced() map[string]SyncthingDevice {
	introduced := map[string]SyncthingDevice{}
	for _, device := range st.Config.Devices {
		if device.IntroducedBy != "" {
			introduced[device.DeviceID] = device
		}
	}
	return introduced
}

// PeerListContainsIntroduced Returns 'true' if the given peerList contains a node
// which has been introduced to us by another Syncthing instance, 'false' otherwise.
func (st *Syncthing) PeerListContainsIntroduced(peerList []v1alpha1.SyncthingPeer) bool {
	introducedSet := st.collectIntroduced()

	// check if the peerList contains an introduced node
	for _, peer := range peerList {
		if _, ok := introducedSet[peer.ID]; ok {
			return true
		}
	}
	return false
}

// PeerListContainsSelf Returns 'true' if the given peerList contains the self node, 'false' otherwise.
func (st *Syncthing) PeerListContainsSelf(peerList []v1alpha1.SyncthingPeer) bool {
	for _, peer := range peerList {
		if peer.ID == st.SystemStatus.MyID {
			return true
		}
	}
	return false
}

// GetIntroducedAndSelfFromFolder Returns a a list of those FolderDeviceConfiguration
// objects which are either us or have been introduced by us.
func (st *Syncthing) GetIntroducedAndSelfFromFolder(folder SyncthingFolder) []FolderDeviceConfiguration {
	// filter out the devices which are not us or have been introduced by us
	var devices []FolderDeviceConfiguration
	for _, device := range folder.Devices {
		if device.DeviceID == st.SystemStatus.MyID || device.IntroducedBy != "" {
			devices = append(devices, device)
		}
	}
	return devices
}

// GetDeviceFromID Returns the device with the given ID,
// along with a boolean indicating whether the device was found.
func (st *Syncthing) GetDeviceFromID(deviceID string) (SyncthingDevice, bool) {
	for _, device := range st.Config.Devices {
		if device.DeviceID == deviceID {
			return device, true
		}
	}
	return SyncthingDevice{}, false
}

// FetchLatestInfo Updates the Syncthing object with the latest data fetched from the Syncthing API.
func (st *Syncthing) FetchLatestInfo() error {
	if err := st.FetchSyncthingConfig(); err != nil {
		return err
	}
	if err := st.FetchSyncthingSystemStatus(); err != nil {
		return err
	}
	if err := st.FetchConnectedStatus(); err != nil {
		return err
	}
	return nil
}

// UpdateSyncthingConfig Updates the Syncthing config with the locally-stored config.
func (st *Syncthing) UpdateSyncthingConfig() error {
	// update the config
	st.logger.V(4).Info("Updating Syncthing config")
	_, err := st.jsonRequest("/rest/config", "PUT", st.Config)
	if err != nil {
		st.logger.V(4).Error(err, "Failed to update Syncthing config")
		return err
	}
	return err
}

// FetchSyncthingConfig fetches the Syncthing config and updates the config.
func (st *Syncthing) FetchSyncthingConfig() error {
	responseBody := &SyncthingConfig{
		Devices: []SyncthingDevice{},
		Folders: []SyncthingFolder{},
	}
	st.logger.V(4).Info("Fetching Syncthing config")
	data, err := st.jsonRequest("/rest/config", "GET", nil)
	if err != nil {
		return err
	}
	err = json.Unmarshal(data, responseBody)
	st.Config = responseBody
	return err
}

// FetchSyncthingSystemStatus fetches the Syncthing system status.
func (st *Syncthing) FetchSyncthingSystemStatus() error {
	responseBody := &SystemStatus{}
	st.logger.V(4).Info("Fetching Syncthing system status")
	data, err := st.jsonRequest("/rest/system/status", "GET", nil)
	if err != nil {
		return err
	}
	// unmarshal the data into the responseBody
	err = json.Unmarshal(data, responseBody)
	st.SystemStatus = responseBody
	return err
}

// FetchConnectedStatus Fetches the connection status of the syncthing instance.
func (st *Syncthing) FetchConnectedStatus() error {
	// updates the connected status if successful, else returns an error
	responseBody := &SystemConnections{
		Connections: map[string]ConnectionStats{},
	}
	st.logger.V(4).Info("Fetching Syncthing connected status")
	data, err := st.jsonRequest("/rest/system/connections", "GET", nil)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, responseBody); err == nil {
		st.SystemConnections = responseBody
	}
	return err
}

// GetDeviceName Returns the name of the device with the given ID, if one is provided.
func (st *Syncthing) GetDeviceName(deviceID string) string {
	for _, device := range st.Config.Devices {
		if device.DeviceID == deviceID {
			return device.Name
		}
	}
	return ""
}

// jsonRequest performs a request to the Syncthing API and returns the response body.
//nolint:funlen,lll,unparam,unused
func (st *Syncthing) jsonRequest(endpoint string, method string, requestBody interface{}) ([]byte, error) {
	// marshal above json body into a string
	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}
	// tostring the json body
	body := io.Reader(bytes.NewReader(jsonBody))

	// build new client if none exists
	req, err := http.NewRequest(method, st.APIConfig.APIURL+endpoint, body)
	if err != nil {
		return nil, err
	}

	// set headers
	headers, err := st.APIConfig.Headers()
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	// make an HTTPS POST request
	if err != nil {
		return nil, err
	}
	resp, err := st.APIConfig.Client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, errors.New("HTTP status code is not 200")
	}

	// read body into response
	return ioutil.ReadAll(resp.Body)
}

// Headers Returns a map containing the necessary headers for Syncthing API requests.
// When no API Key is provided, an error is returned.
func (api *APIConfig) Headers() (map[string]string, error) {
	if api.APIKey == "" {
		return nil, errors.New("API Key is not set")
	}

	return map[string]string{
		"X-API-Key":    api.APIKey,
		"Content-Type": "application/json",
	}, nil
}

// BuildTLSClient Returns a new TLS client for Syncthing API requests.
func (api *APIConfig) BuildOrUseExistingTLSClient() *http.Client {
	if api.Client != nil {
		return api.Client
	}
	return api.BuildTLSClient()
}

// BuildTLSClient Returns a new TLS client for Syncthing API requests.
func (api *APIConfig) BuildTLSClient() *http.Client {
	tlsConfig := api.TLSConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	// load the TLS config with certificates
	tr := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   time.Second * 5,
	}
	return client
}

// GenerateRandomBytes Generates random bytes of the given length using the OS's RNG.
func GenerateRandomBytes(length int) ([]byte, error) {
	// generates random bytes of given length
	b := make([]byte, length)
	_, err := rand.Read(b)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// GenerateRandomString Generates a random string of the given length using the OS's RNG.
func GenerateRandomString(length int) (string, error) {
	// generate a random string
	b, err := GenerateRandomBytes(length)
	return base64.URLEncoding.EncodeToString(b), err
}

// AsTCPAddress Accepts a partial URL which may be a hostname or a hostname:port, and returns a TCP address.
// The provided address should NOT have a protocol prefix.
func AsTCPAddress(addr string) string {
	// check if TCP is already prefixed
	if strings.HasPrefix(addr, "tcp://") {
		return addr
	}
	return "tcp://" + addr
}
