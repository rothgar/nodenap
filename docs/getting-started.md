# Getting started

This walks through installing nodenap, describing one machine, watching
it in dry run, and turning it loose.

## Before you start

You need, for every machine you want powered off:

1. **A way to turn it on** while it's off: Wake-on-LAN, a smart plug (with the
   BIOS set to power on when AC power returns), or a BMC (IPMI/Redfish).
   Test it by hand first. See [power drivers](power-drivers.md).
2. **A way to shut it down cleanly**: the Talos API, Omni, SSH, a BMC's ACPI
   soft-off, or an HTTP call.
3. Ideally **a power reading**: a metering plug, the BMC, or a Prometheus
   query. Without one the scaler uses the `profile` watts you give it. See
   [power sensors](power-sensors.md).
4. **Your electricity price**. See [finding your power costs](power-costs.md).

Never power off your control plane. nodenap refuses to anyway.

## 1. Install

```sh
kubectl create namespace nodenap
kubectl label namespace nodenap pod-security.kubernetes.io/enforce=privileged
helm install nodenap oci://ghcr.io/rothgar/charts/nodenap -n nodenap
```

The label lets the controller use `hostNetwork` (for Wake-on-LAN broadcasts)
on clusters that enforce Pod Security, such as Talos. Without it the
Deployment never gets a pod. Don't use Wake-on-LAN? Skip the label and add
`--set hostNetwork=false --create-namespace`.

The chart installs the CRDs, the controller and a default PowerPolicy in
**dry run**. Useful values (`--set key=value`):

| Value | Default | |
|---|---|---|
| `policy.price` | `"0.20"` | your all-in price per kWh |
| `policy.timeZone`, `policy.currency` | | |
| `policy.dryRun` | `true` | |
| `policy.create` | `true` | set `false` to manage the PowerPolicy yourself |
| `hostNetwork` | `true` | needed for Wake-on-LAN broadcasts |
| `image.tag` | chart version | |

The controller prefers a control-plane node and runs anywhere if there
isn't one. With `hostNetwork`, an upgrade can land it on another node (the
old pod holds the host ports until it exits); if you have an always-on node,
make the `affinity` *required* to keep it there. It can power off the node it runs on: it drains everything else
first, evicts itself last, and the replacement pod on another node picks up
from the Machine status and finishes the power-off. With `hostNetwork`,
Wake-on-LAN broadcasts reach your LAN; if your machines are on a different
subnet, use a directed broadcast address (e.g. `192.168.1.255`) in the
`wakeOnLAN` driver.

Helm installs CRDs only on first install. After upgrading to a version with
API changes, apply them from `charts/nodenap/crds/`. With a Flux
HelmRelease, set `install.crds: CreateReplace` and `upgrade.crds:
CreateReplace` and Flux does it for you.

Without Helm: `kubectl apply -k config/default` (set the image in
`config/manager/manager.yaml`).

## 2. Create Secrets

Drivers read credentials from Secrets in the scaler's namespace
(`nodenap`). The scaler can only read Secrets there; it never watches
Secrets cluster-wide.

```sh
kubectl -n nodenap create secret generic talosconfig --from-file=talosconfig=./talosconfig
kubectl -n nodenap create secret generic shelly --from-literal=password=...
```

Each driver's page lists the keys it expects.

## 3. A policy and a machine

The chart created a dry-run policy. Within ~15 minutes
`kubectl get powerpolicy default -o yaml` lists `status.suggestions`: nodes
that could sleep, even before any Machine exists. Set your price:

```yaml
apiVersion: power.nodenap.rothgar.github.io/v1alpha1
kind: PowerPolicy
metadata: {name: default}
spec:
  dryRun: true
  energy:
    tariff:
      defaultPrice: "0.20"
```

and one `Machine` per physical machine, named after its Kubernetes node:

```yaml
apiVersion: power.nodenap.rothgar.github.io/v1alpha1
kind: Machine
metadata: {name: worker-1}
spec:
  powerOn:
    - wakeOnLAN: {mac: "aa:bb:cc:dd:ee:01"}
  shutdown:
    talos: {talosconfigSecretRef: {name: talosconfig}}
  profile: {idleWatts: "15"}
```

Nodes without a Machine still count as capacity; they're just never powered
off. Complete setups are in [examples](examples/).

## 4. Watch it decide

```sh
kubectl get powerpolicy -o wide   # PLANNED / SUGGESTED: what it would do now
kubectl get machines              # WOULD: dry-run decision; IDLE: unneeded for how long
kubectl get powerpolicy default -o yaml
kubectl get events -A --field-selector reason=WouldPowerOff
```

```
NAME      DRYRUN   ON  OFF  WATTS   PRICE    COST/H   PLANNED   SUGGESTED          LAST RUN
default   true     9   0    756.4   0.3883   0.2937   qb2=Off   pi4-1=Off,pi4-2=Off  9s

NAME   NODE   STATE  TARGET  WOULD  WATTS  IDLE
qb2    qb2    On     On      Off    582.6  16m
spark  spark  On     On             56.5
```

A machine shows up after it has been unneeded for `scaleDown.unneededTime`
(15 minutes). `status.lastActions` says what it would do and why;
`status.unsatisfiablePods` lists pending pods that no machine (on or off) can
run; `status.warnings` lists problems reading prices or sensors.

Leave it in dry run for a day or two. Check that:

- the price in `status.price` matches your tariff at different times of day;
- measured watts (`kubectl get machines`) look right;
- the machines it wants to power off really are idle, and the ones it would
  wake really are the right ones.

You can also try scenarios without installing anything:

```sh
go run ./cmd/powerplan -f my-config.yaml \
  --prometheus-service monitoring/prometheus-operated:9090 \
  --assume-off worker-2 \
  --pending what-if-pods.yaml
```

`--assume-off` pretends machines are off; `--pending` adds hypothetical
unschedulable pods. If the policy uses Home Assistant, put the token in
`HA_TOKEN`. Secrets are never read from the cluster.

## 5. Go live

Before setting `dryRun: false`:

- **Test every machine's power-on by hand** from fully off, the way the
  scaler will do it (send the magic packet, switch the plug on, use the BMC).
- **Give big workloads realistic memory requests.** The scheduler and the
  scaler both use requests. A GPU or LLM server that requests nothing looks
  tiny, so the scaler may plan to squeeze it onto another machine.
- **Mark per-machine helpers** (hardware managers, exporters deployed as
  Deployments) in `ignorePods`, or they'll keep their machine on forever.
- **Check PodDisruptionBudgets.** A drain that can't finish within
  `scaleDown.drainTimeout` is abandoned and the machine stays on.

Then set `dryRun: false`. Force a machine on or off at any time with:

```sh
kubectl annotate machine worker-1 nodenap.rothgar.github.io/override=on --overwrite
kubectl annotate machine worker-1 nodenap.rothgar.github.io/override-     # back to automatic
```
