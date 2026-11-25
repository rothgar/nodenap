# Power drivers

A **driver** is one way of acting on a machine. Each `Machine` uses drivers in
four places:

| Field | When it's used |
|---|---|
| `powerOn` (list) | to switch the machine on; tried in order, each gets `bootTimeout` to make the Node Ready |
| `shutdown` | to ask the OS to shut down cleanly, after the node is drained |
| `powerCut` | optional: cuts power once the node is NotReady (after `powerCutDelay`), or if shutdown times out |
| `sensor` | to read watts (see [power sensors](power-sensors.md)) |

On and off are independent. "Shut down with Talos, cut the plug, power on with
the plug" and "shut down through Omni, power on with Wake-on-LAN" are both
fine. Not every driver works in every slot:

| | powerOn | shutdown | powerCut | sensor |
|---|:-:|:-:|:-:|:-:|
| `wakeOnLAN` | ✓ | | | |
| `http` | ✓ | ✓ | ✓ | ✓ |
| `ipmi` | ✓ | ✓ (ACPI soft-off) | ✓ | ✓ (DCMI) |
| `redfish` | ✓ | ✓ | ✓ | ✓ |
| `omni` | infra-provider machines only | ✓ | | |
| `talos` | | ✓ | | |
| `ssh` | | ✓ | | |
| `prometheus` | | | | ✓ |

Secrets live in the scaler's namespace (`nodenap`).

## Choosing a combination

| Your hardware | powerOn | shutdown | powerCut |
|---|---|---|---|
| Mini PC / desktop, no plug | `wakeOnLAN` | `talos` / `omni` / `ssh` | none |
| Anything on a smart plug | the plug (`http`) | `talos` / `omni` / `ssh` | the plug |
| Server with a BMC | `redfish` or `ipmi` | same BMC | same BMC |
| Raspberry Pi | a plug (Pis boot when power is applied) | `talos` / `ssh` | the plug |

**Never cut power to a machine that wakes with Wake-on-LAN.** WoL needs
standby power, so put a `powerCut` only on machines that power on another way.

### Smart-plug machines need "power on after AC loss"

For a plug to turn a machine *on*, the firmware must boot when power returns.
In the BIOS/UEFI look for *Restore on AC Power Loss*, *AC Back* or *After Power
Failure*, and set it to **Power On**. Test it: shut the machine down, switch
the plug off and on, and check it boots.

## wakeOnLAN

```yaml
powerOn:
  - wakeOnLAN:
      mac: "aa:bb:cc:dd:ee:01"
      broadcast: 192.168.1.255   # optional; default 255.255.255.255
      port: 9                    # optional
```

Checklist:

- Enable **Wake on LAN** (sometimes *Power On by PCI-E*) in the BIOS, and
  disable *ErP/EuP* deep sleep, which cuts NIC standby power.
- The NIC must be armed for magic packets when the OS shuts down. On most
  Linux distros: `ethtool -s eth0 wol g`, made persistent with
  `WakeOnLan=magic` in a systemd `.link` file or in NetworkManager. Many NICs
  report wake as disabled in Linux but still wake from a full power-off via
  the BIOS setting. **Test from fully off before relying on it.**
- The scaler pod must reach the machine's L2 network: the chart uses
  `hostNetwork`. Across subnets, use a directed broadcast address that your
  router forwards.
- Sending the packet repeatedly is fine; the scaler re-sends it every loop
  until the node is Ready.

## http

One HTTP request per slot. Actions default to `POST`, readings to `GET`.

```yaml
http:
  method: GET                      # optional
  url: http://192.168.1.50/rpc/Switch.Set?id=0&on=true
  body: '{"entity_id": "switch.x"}' # optional; JSON bodies get Content-Type: application/json
  headers: {X-Thing: value}        # optional
  headersSecretRef: {name: ha}     # optional: every key in the Secret becomes a header
  auth:                            # optional: basic or digest; Secret has username (default "admin") and password
    type: digest
    secretRef: {name: shelly}
  insecureSkipTLSVerify: false
  field: apower                    # readings only: dot path into the JSON response
  scale: "1"                       # readings only
```

### Shelly Gen2+ (Plus, Pro, Gen3)

```yaml
powerOn:
  - http: {method: GET, url: "http://PLUG/rpc/Switch.Set?id=0&on=true"}
powerCut:
  http: {method: GET, url: "http://PLUG/rpc/Switch.Set?id=0&on=false"}
sensor:
  http: {url: "http://PLUG/rpc/Switch.GetStatus?id=0", field: apower}
```

If the plug has a password, add `auth: {type: digest, secretRef: {name: shelly}}`
(Secret key `password`; the username is always `admin`).

### Shelly Gen1

```yaml
powerOn:
  - http: {method: GET, url: "http://PLUG/relay/0?turn=on"}
powerCut:
  http: {method: GET, url: "http://PLUG/relay/0?turn=off"}
sensor:
  http: {url: "http://PLUG/status", field: meters.0.power}
```

Password-protected Gen1 devices use `auth: {type: basic, ...}`.

### Tasmota

```yaml
powerOn:
  - http: {method: GET, url: "http://PLUG/cm?cmnd=Power%20On"}
powerCut:
  http: {method: GET, url: "http://PLUG/cm?cmnd=Power%20Off"}
sensor:
  http: {url: "http://PLUG/cm?cmnd=Status%208", field: StatusSNS.ENERGY.Power}
```

With a web password, add `&user=admin&password=...` to the URLs.

### Home Assistant (TP-Link/Kasa, Zigbee/Z-Wave plugs, anything with a switch)

Create a long-lived access token (your HA profile → Security) and a Secret
with an `Authorization` key:

```sh
kubectl -n nodenap create secret generic ha-headers \
  --from-literal=Authorization="Bearer <token>"
```

```yaml
powerOn:
  - http:
      url: http://homeassistant.home:8123/api/services/switch/turn_on
      body: '{"entity_id": "switch.server_plug"}'
      headersSecretRef: {name: ha-headers}
powerCut:
  http:
    url: http://homeassistant.home:8123/api/services/switch/turn_off
    body: '{"entity_id": "switch.server_plug"}'
    headersSecretRef: {name: ha-headers}
sensor:
  http:
    url: http://homeassistant.home:8123/api/states/sensor.server_plug_power
    field: state
    headersSecretRef: {name: ha-headers}
```

The same pattern drives Home Assistant scripts (`/api/services/script/...`),
PDUs, or any custom webhook.

## ipmi

IPMI v2.0 over LAN (lanplus).

```yaml
powerOn:
  - ipmi: {address: 192.168.10.31, credentialsSecretRef: {name: bmc}}   # chassis power on
shutdown:
  ipmi: {address: 192.168.10.31, credentialsSecretRef: {name: bmc}}     # ACPI soft-off
powerCut:
  ipmi: {address: 192.168.10.31, credentialsSecretRef: {name: bmc}}     # chassis power off
sensor:
  ipmi: {address: 192.168.10.31, credentialsSecretRef: {name: bmc}}     # DCMI power reading
```

Secret keys: `username`, `password`. Create a dedicated BMC user with
operator privileges. `ipmitool -I lanplus -H ... -U ... -P ... chassis status`
is a good test. Not every BMC supports DCMI power readings.

## redfish

For modern BMCs: Dell iDRAC, HPE iLO, Lenovo XClarity, Supermicro, OpenBMC.

```yaml
redfish:
  address: https://192.168.10.21
  credentialsSecretRef: {name: idrac}   # username, password
  insecureSkipTLSVerify: true           # BMCs usually have self-signed certs
  systemId: System.Embedded.1           # optional: default is the first system
  chassisId: System.Embedded.1          # optional: default is the first chassis
```

powerOn sends `On`, shutdown sends `GracefulShutdown`, powerCut sends
`ForceOff`. Sensor readings come from the chassis `Power` resource or the
newer `EnvironmentMetrics` resource. YAML anchors keep it short; see
[bmc-servers-redfish-ipmi.yaml](examples/bmc-servers-redfish-ipmi.yaml).

## talos

```yaml
shutdown:
  talos:
    talosconfigSecretRef: {name: talosconfig}   # key "talosconfig" by default
    endpoint: 192.168.1.21                       # optional: default is the node's InternalIP
```

Generate a talosconfig with only the operator role:

```sh
talosctl config new --roles os:operator ./talosconfig
kubectl -n nodenap create secret generic talosconfig --from-file=talosconfig=./talosconfig
```

## omni

For Talos clusters managed by [Sidero Omni](https://www.siderolabs.com/omni/).
Omni performs the shutdown, so no talosconfig is needed.

```yaml
shutdown:
  omni:
    credentialsSecretRef: {name: omni}
    machineId: ""   # optional: default is the machine whose node name matches
```

```sh
omnictl serviceaccount create nodenap --use-user-role=false --role Operator
kubectl -n nodenap create secret generic omni \
  --from-literal=endpoint=https://<account>.omni.siderolabs.io \
  --from-literal=serviceAccountKey=<key from the command above>
```

Service account keys expire after a year by default (`--ttl`).

With an `omni` driver configured, nodenap also pauses power-offs and wakes
every machine while Omni upgrades the cluster
([upgrades](how-it-works.md#upgrades)).

`omni` can also be used in `powerOn`, but only for machines run by an Omni
infrastructure provider (such as the bare-metal provider). For any other
machine, pair `omni` shutdown with Wake-on-LAN, a plug or a BMC to power on.

## ssh

For distros with a shell (k3s, kubeadm, MicroK8s on Ubuntu/Debian/Fedora...).

```yaml
shutdown:
  ssh:
    user: scaler                       # default root
    command: sudo systemctl poweroff   # default "poweroff"
    credentialsSecretRef: {name: ssh-shutdown}
    address: 192.168.1.12              # optional: default is the node's InternalIP
    insecureIgnoreHostKey: false
```

Secret keys: `privateKey`, and `knownHosts` (strongly recommended; without it
you must set `insecureIgnoreHostKey: true`). A least-privilege setup:

```
# /etc/sudoers.d/nodenap
scaler ALL=(root) NOPASSWD: /usr/bin/systemctl poweroff
```

## What isn't supported

- **Suspend / sleep.** Suspend-to-RAM would resume in seconds, but in testing
  it was not dependable enough to automate: some platforms only offer
  `s2idle`, which saved ~8 W of 56 W on an NVIDIA GB10, and Wake-on-LAN did
  not resume it. Check `/sys/power/mem_sleep` on your hardware if you want to
  experiment by hand.
- **Intel AMT.** Use IPMI or Redfish if your board has them, or a smart plug.
