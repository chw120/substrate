# Reproducing the DurDir size sweep on micro-VM

End-to-end procedure for the measurement in
[baseline-2026-08-26.md](baseline-2026-08-26.md): actor suspend/resume latency
against DurableDir volume size, on the micro-VM sandbox class.

The scenarios themselves live in
[`../automation/tests.yaml`](../automation/tests.yaml) as `durdir_size_*`. In
normal operation `benchmarking/automation/orchestrator.py` runs the whole
suite on a schedule and handles setup and teardown per test. This document
covers driving individual scenarios by hand against a cluster you keep alive
between runs, which is what you want while iterating.

> [!IMPORTANT]
> Two failure modes below appear only at 500 MiB and above, and neither is
> obvious from the error message. Read [Gotchas](#gotchas) before your first
> run rather than after it.

## Prerequisites

* `gcloud` (authenticated: `gcloud auth login`), `kubectl`, `ko`, `python3`.
* `gke-gcloud-auth-plugin` — `gcloud components install gke-gcloud-auth-plugin`.
* `envsubst`, used by `benchmarking/locust/deploy.sh`. Not present on macOS by
  default: `brew install gettext` and put
  `$(brew --prefix gettext)/bin` on `PATH`.
* `zstd`, used by `hack/install-microvm-deps.sh` to unpack the Kata assets.
  `brew install zstd`. Without it the unpack fails with
  `tar: ... unable to run program "zstd -d -qq"`.
* `pyyaml`, for the orchestrator helpers.
* A container build path — see [Building the Locust image](#building-the-locust-image).

Create a `.ate-dev-env.sh` at the repo root from
`hack/ate-dev-env.sh.example` and `source` it before every step. It is
gitignored. The runs in the baseline used:

```bash
export PROJECT_ID=<your-project>
export GCE_REGION=us-central1
export CLUSTER_LOCATION=us-central1-a
export CLUSTER_NAME=substrate-poc
export CLUSTER_VERSION=1.35.7-gke.1222000
export GVISOR_NODE_MACHINE_TYPE=n2-standard-4
export BUCKET_NAME=snapshot-substrate-test-${PROJECT_ID}
export KO_DOCKER_REPO="gcr.io/${PROJECT_ID}/ate-images"
export KO_DEFAULTPLATFORMS=linux/amd64
```

`CLUSTER_VERSION` must be a version GKE currently offers in your zone; the
example file's pinned value goes stale. Check with
`gcloud container get-server-config --zone "$CLUSTER_LOCATION"`.

## 1. Cluster

```bash
source .ate-dev-env.sh
go run ./tools/setup-gcp bootstrap
```

This creates the cluster, the snapshot bucket, the Workload Identity bindings
and the monitoring dashboards.

**`setup-gcp` cannot produce a micro-VM-capable node pool.** It creates only
`substrate-node-pool` with no nested virtualization, no labels and no taints
(see `tools/setup-gcp/cmd/cluster.go`). Micro-VM workers need all three, so
add the pool by hand. The convention — `ate.dev/sandboxClass=microvm` as both
label and `NoSchedule` taint — is what `atecontroller` schedules against; see
`cmd/atecontroller/internal/controllers/workerpool_apply.go`, which also
mounts `/dev/kvm` from the host.

```bash
gcloud container node-pools create microvm-pool \
  --cluster="$CLUSTER_NAME" --zone="$CLUSTER_LOCATION" \
  --machine-type=n2-standard-4 \
  --num-nodes=1 \
  --disk-size=100 \
  --image-type=UBUNTU_CONTAINERD \
  --enable-nested-virtualization \
  --node-labels=ate.dev/sandboxClass=microvm \
  --node-taints=ate.dev/sandboxClass=microvm:NoSchedule
```

Not every machine family and zone combination has capacity. `c3-standard-4` in
`us-central1-c` returned `GCE_STOCKOUT` after a 35-minute wait — which is a
capacity error, not a quota error, so checking your quota will mislead you.
`n2-standard-4` in `us-central1-a` worked and is the family the repo docs name
as known-good for nested virtualization.

Confirm KVM actually reached the node before going further:

```bash
kubectl get nodes -L ate.dev/sandboxClass
kubectl debug node/<microvm-node> -it --image=busybox -- ls -l /host/dev/kvm
```

## 2. Substrate and micro-VM assets

```bash
./hack/install-ate.sh --deploy-ate-system
./hack/install-microvm-deps.sh --install     # stages Kata + Cloud Hypervisor, applies the microvm SandboxConfig
kubectl -n ate-system get pods               # all Running before continuing
```

Order matters: `install-microvm-deps.sh` applies a `SandboxConfig` CR and needs
the CRDs that `--deploy-ate-system` installs.

## 3. Workloads

```bash
./benchmarking/workloads/deploy.sh --deploy --worker-count 1 --sandbox-class microvm
kubectl wait --for=condition=Ready --all actortemplates -n benchmark-workloads --timeout=300s
```

Verify the worker landed on the micro-VM node and the templates picked up the
class:

```bash
kubectl -n benchmark-workloads get pods -o wide
kubectl -n benchmark-workloads get actortemplates -o custom-columns=NAME:.metadata.name,CLASS:.spec.sandboxClass
kubectl -n benchmark-workloads get workerpool
```

Keep `--worker-count 1`. A worker hosts exactly one actor, so one worker and
one user is the configuration that isolates latency from queueing.

**`--sandbox-class` here is what actually decides where actors run.** The
`sandboxClass` field in `tests.yaml` only tells the orchestrator how to deploy
workloads; it is not consulted at test time. When driving scenarios by hand,
whatever you deployed in this step is what you measure, regardless of which
scenario name you pass. So keep the two in agreement — running a `_microvm`
scenario against a gvisor deployment will happily produce gvisor numbers under
a micro-VM label.

## 4. Building the Locust image

```bash
./benchmarking/locust/build_and_push.sh
```

This needs a working Docker daemon that can reach Docker Hub. If it cannot —
a colima VM with broken outbound networking will fail on
`FROM python:3.11-slim` with `dial tcp ...: i/o timeout` while the host
network is fine — build in Cloud Build instead, which needs no local daemon:

```bash
gcloud services enable cloudbuild.googleapis.com --project="$PROJECT_ID"

cat > /tmp/cloudbuild-locust.yaml <<'YAML'
steps:
  - name: gcr.io/cloud-builders/docker
    args: [build, -t, '${_IMAGE}', -f, benchmarking/locust/Dockerfile, .]
images: ['${_IMAGE}']
options:
  machineType: E2_HIGHCPU_8
timeout: 3600s
YAML

# Keep the upload small; the build context is the monorepo root.
printf '.git\n.claude\n.ate-dev-env.sh\nbin\n*.log\n' > .gcloudignore

gcloud builds submit --project="$PROJECT_ID" --region=us-central1 \
  --config=/tmp/cloudbuild-locust.yaml \
  --substitutions=_IMAGE="us-docker.pkg.dev/${PROJECT_ID}/gcr.io/ate-images/locust-test:latest" .
```

Roughly 2.5 minutes. The image name must match what
`benchmarking/locust/build_and_push.sh` would have produced, because the
runner Job template is given the same string.

## 5. Raise the router's route timeout (required for 1 GiB)

Envoy's route timeout defaults to 10 s (`defaultRouteTimeout` in
`cmd/atenet/internal/router/xds.go`). A 1 GiB `DurDirWrite` takes ~16 s and
`DurDirOverwrite` ~11 s, so without this every iteration returns
`HTTP 504: upstream request timeout`.

Pass it to the installer, which applies it to `deployment/atenet-router` after
the manifest lands and re-applies it on every `--deploy-atenet`:

```bash
./hack/install-ate.sh --route-timeout 5m --deploy-ate-system
```

Against a cluster that is already up and that you do not want to reinstall, the
same patch by hand:

```bash
kubectl -n ate-system patch deploy atenet-router --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--route-timeout=5m"}]'
kubectl -n ate-system rollout status deploy/atenet-router --timeout=300s
```

The hand patch is imperative and will not survive `--delete-ate-system` or an
orchestrator redeploy; the flag will.

Sizes up to 500 MiB do not need this — but 500 MiB writes land at ~6.9 s
against a 10 s ceiling, which is close enough that a slow day could flake.

## 6. Running a scenario

The orchestrator's full lifecycle tears down and redeploys substrate and the
workloads for every test, which is correct for scheduled runs and far too slow
for iteration. This driver reuses its rendering and job-waiting code against
the cluster you already have:

```python
# /tmp/run_one.py
# usage: python3 /tmp/run_one.py <test-name> <image> <dest> [duration-override]
import os, subprocess, sys, time
sys.path.insert(0, os.path.join(os.getcwd(), "benchmarking/automation"))
import yaml, orchestrator

name, image, dest = sys.argv[1], sys.argv[2], sys.argv[3]
duration = sys.argv[4] if len(sys.argv) > 4 else None

tests = yaml.safe_load(open("benchmarking/automation/tests.yaml"))["tests"]
test = next(t for t in tests if t["name"] == name)
if duration:
    test = dict(test, duration=duration)

# A worker hosts one actor at a time, and a run that exits before deleting its
# actor leaves the assignment behind, starving the next run. The orchestrator
# redeploys workloads per test; here the pool is recycled instead.
subprocess.run(["kubectl", "-n", "benchmark-workloads", "delete", "pod",
                "-l", "ate.dev/worker-pool=benchmark-ateom", "--wait=true"], check=True)
for _ in range(60):
    r = subprocess.run(["kubectl", "-n", "benchmark-workloads", "get", "workerpool",
                        "benchmark-ateom", "-o", "jsonpath={.status.readyReplicas}"],
                       capture_output=True, text=True, check=False)
    if r.stdout.strip() == "1":
        break
    time.sleep(5)
else:
    raise RuntimeError("worker pool did not become ready")

result = orchestrator.run_test(test, image, dest, commit="manual0",
                               manifests_dir="benchmarking/automation/manifests")
print(f"RESULT={result}")
sys.exit(0 if result == "complete" else 1)
```

Then:

```bash
source .ate-dev-env.sh
IMG="us-docker.pkg.dev/${PROJECT_ID}/gcr.io/ate-images/locust-test:latest"

for t in durdir_size_5mb_microvm durdir_size_10mb_microvm durdir_size_64mb_microvm \
         durdir_size_500mb_microvm durdir_size_1gb_microvm; do
  python3 /tmp/run_one.py "$t" "$IMG" /tmp/results
done
```

Swap the `_microvm` suffix off the names to sweep gvisor instead, after
redeploying the workloads with `--sandbox-class gvisor` in step 3.

Passing a trailing duration (`... /tmp/results 5m`) overrides the scenario's
own. The committed durations are what you want for a quotable baseline; see
[Sample counts](#sample-counts).

`--dest` accepts a local path or a `gs://` URL. With a local path the CSVs stay
inside the Job's pod and vanish with it, which is fine because the numbers are
in the Job log; point it at a bucket if you want the artifacts.

## 7. Reading the results

`orchestrator.run_test` prints the Job log, which ends with locust's summary
and its percentile table. To pull the final table out of a captured log:

```python
import re
body = open("run.log").read().splitlines()
idx = max(i for i, l in enumerate(body) if "Response time percentiles" in l)
for l in body[idx:idx + 16]:
    l = l.replace("[locust] ", "")
    if re.search(r"ResumeActor|SuspendActor|DurDir", l):
        p = l.split()
        # columns: 50 66 75 80 90 95 98 99 99.9 99.99 100, then #reqs
        print(f"{p[1]:<24} p50={p[2]:>6} p95={p[7]:>6} p100={p[12]:>6} n={p[13]}")
```

Take the **last** table in the log: locust prints an interim one every few
seconds, and grepping naively gives you a mid-run snapshot.

For the server-side decomposition — where resume time actually goes — read the
ateom worker log. `Actor boot phases` breaks out `agent_dial`, `containers` and
`readyz`; `Actor checkpointed` breaks out `pause`, `durable_dir` and
`teardown`, in nanoseconds:

```bash
kubectl -n benchmark-workloads logs -l ate.dev/worker-pool=benchmark-ateom --tail=2000 \
  | grep -E "Actor boot phases|Actor checkpointed|Actor restored"
```

The metric instruments carry `ate.snapshot.phase` and
`ate.actor.operation.name` attributes; `docs/metrics/registry/metrics.yaml` is
the registry of what exists.

## Gotchas

**`ResourceExhausted: no free workers available` on every iteration.** A worker
hosts exactly one actor — `cmd/ateapi/internal/scheduling/scheduling.go` treats
any worker whose `Status.Assignment` is non-nil as busy. A run that exits
before deleting its actor leaks the assignment, and with one worker the next
run fails 100% of iterations, instantly. The tell is that the failures start
immediately rather than after a warm-up, and `CreateActor` succeeds while
`ResumeActor` fails. Recycle the WorkerPool between runs (the driver above
does).

**`HTTP 504: upstream request timeout` on `DurDirWrite` / `DurDirOverwrite`.**
Envoy's route timeout. See [step 5](#5-raise-the-routers-route-timeout-required-for-1-gib).

**Do not use `benchmarking/deploy_locust.sh` for DurDir work.** It calls
`locust/deploy.sh --deploy` without `--user-class`, so it silently deploys the
`glutton` class instead of `durdir`. Run the steps separately, or pass
`--user-class durdir` to `locust/deploy.sh` yourself. This only matters for the
long-lived web-UI deployment; the runner Jobs in step 6 take the class from the
scenario's `file:` field and are unaffected.

**Digest read mode is mandatory above ~64 MiB, not merely cheaper.** A
`data`-mode `ReadDisk` does `io.ReadAll` on the file
(`cmd/benchmarking/glutton/main.go`) and ships it over the wire, so at 500 MiB
and 1 GiB it would exhaust the actor and measure the router rather than the
restore. Every scenario in the sweep passes `--durdir-read-mode digest`.

**`--durdir-file-size-bytes` is capped at `math.MaxInt32`** (2 GiB) by
`internal/benchmarking/boomer/dynconfig/dynconfig.go`. Sizes beyond that need a
wire-format change, not just a bigger number.

**The `--durdir-*` flags are not settable from the Locust web UI.** They are
registered as plain locust CLI arguments in
`benchmarking/locust/common/durdir_config.py` without `include_in_web_ui`, so
the master reads them from its command line. Boomer workers fetch the values
from the master's `/boomer-config` endpoint. Driving these scenarios means a
headless run with the flags on the command line — which is what the runner Job
does.

## Sample counts

At 1 concurrent user with a 1.0 s fixed wait, iteration rate is set by how long
one suspend/resume cycle takes, so the same wall-clock duration yields very
different sample counts across the sweep:

| Size | measured rate | committed micro-VM duration | expected n |
|---|---|---|---|
| 5 MiB | ~100 in 5m | 3m | ~60 |
| 10 MiB | ~100 in 5m | 3m | ~60 |
| 64 MiB | ~54 in 5m | 5m | ~54 |
| 500 MiB | ~12 in 5m | 20m | ~50 |
| 1 GiB | ~11 in 8m | 30m | ~60 |

The micro-VM scenarios run longer than their 1m gvisor counterparts at the same
size because a micro-VM iteration is slower — the cold-boot VMM launch is on
every resume — so the same wall clock buys fewer samples.

At n≈12 locust's p95 is just the slowest sample, and p95, p99 and p100 come out
identical. Locust also rounds to about two significant figures, so at the 1 GiB
end the quantization is ±500 ms. Use the committed durations for anything you
intend to quote.

## Teardown

The cluster bills continuously; three `n2-standard-4` nodes plus the GKE
management fee is not free to leave running.

```bash
./benchmarking/workloads/deploy.sh --delete
./hack/install-microvm-deps.sh --delete    # before --delete-ate-system: it needs the SandboxConfig CRD
./hack/install-ate.sh --delete-ate-system

gcloud container clusters delete "$CLUSTER_NAME" --zone "$CLUSTER_LOCATION" --project "$PROJECT_ID"
```
