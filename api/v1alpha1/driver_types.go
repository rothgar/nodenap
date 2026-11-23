package v1alpha1

// Driver is one way of acting on (or measuring) a machine. Set exactly one
// field. What it does depends on where it is used:
//
//	           powerOn  shutdown  powerCut  sensor
//	wakeOnLAN     ✓
//	http          ✓        ✓         ✓        ✓
//	ipmi          ✓        ✓(soft)   ✓        ✓(DCMI)
//	redfish       ✓        ✓         ✓        ✓
//	omni          ✓*       ✓
//	talos                  ✓
//	ssh                    ✓
//	prometheus                                ✓
//
// * Omni can only power on machines run by an Omni infrastructure provider.
type Driver struct {
	// +optional
	WakeOnLAN *WakeOnLAN `json:"wakeOnLAN,omitempty"`
	// Any HTTP API: smart plugs (Shelly, Tasmota), Home Assistant, PDUs.
	// +optional
	HTTP *HTTPRequest `json:"http,omitempty"`
	// +optional
	IPMI *IPMI `json:"ipmi,omitempty"`
	// +optional
	Redfish *Redfish `json:"redfish,omitempty"`
	// +optional
	Omni *Omni `json:"omni,omitempty"`
	// +optional
	Talos *Talos `json:"talos,omitempty"`
	// +optional
	SSH *SSH `json:"ssh,omitempty"`
	// +optional
	Prometheus *PrometheusSource `json:"prometheus,omitempty"`
}

// WakeOnLAN sends a magic packet. The controller pod needs hostNetwork (or a
// directed broadcast address) to reach the machine's LAN.
type WakeOnLAN struct {
	// +kubebuilder:validation:Pattern=`^([0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}$`
	MAC string `json:"mac"`
	// Defaults to 255.255.255.255.
	// +optional
	Broadcast string `json:"broadcast,omitempty"`
	// Defaults to 9.
	// +optional
	Port int32 `json:"port,omitempty"`
}

// IPMI talks IPMI v2.0 (lanplus) to a BMC.
type IPMI struct {
	Address string `json:"address"`
	// Defaults to 623.
	// +optional
	Port int32 `json:"port,omitempty"`
	// Secret with "username" and "password".
	CredentialsSecretRef SecretRef `json:"credentialsSecretRef"`
}

// Redfish talks to a BMC's Redfish API (iDRAC, iLO, XClarity, OpenBMC, ...).
type Redfish struct {
	// e.g. https://10.0.0.20
	Address string `json:"address"`
	// Defaults to the first ComputerSystem.
	// +optional
	SystemID string `json:"systemId,omitempty"`
	// Defaults to the first Chassis (power readings).
	// +optional
	ChassisID string `json:"chassisId,omitempty"`
	// Secret with "username" and "password".
	CredentialsSecretRef SecretRef `json:"credentialsSecretRef"`
	// BMCs usually have self-signed certificates.
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// Omni shuts a Talos machine down through Sidero Omni.
type Omni struct {
	// Secret with "endpoint" and "serviceAccountKey" (Operator role).
	CredentialsSecretRef SecretRef `json:"credentialsSecretRef"`
	// Defaults to the machine whose Kubernetes node name matches.
	// +optional
	MachineID string `json:"machineId,omitempty"`
}

// Talos shuts a Talos node down through the Talos API.
type Talos struct {
	// Secret key with a talosconfig (os:operator role is enough).
	TalosconfigSecretRef SecretKeyRef `json:"talosconfigSecretRef"`
	// Defaults to the node's InternalIP.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// SSH runs a shutdown command (for k3s, kubeadm, ... on a regular distro).
type SSH struct {
	// host[:port]; defaults to the node's InternalIP.
	// +optional
	Address string `json:"address,omitempty"`
	// Defaults to root.
	// +optional
	User string `json:"user,omitempty"`
	// Secret with "privateKey" and optionally "knownHosts".
	CredentialsSecretRef SecretRef `json:"credentialsSecretRef"`
	// Defaults to "poweroff". Use "sudo systemctl poweroff" for non-root users.
	// +optional
	Command string `json:"command,omitempty"`
	// +optional
	InsecureIgnoreHostKey bool `json:"insecureIgnoreHostKey,omitempty"`
}
