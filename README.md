# k8s-upgrade-report

Turns a directory of Kubernetes manifests into an upgrade readiness report: what
breaks at the target version, what a machine can rewrite for you, what your
vendors have to fix first, and in which order to move the control plane and the
node pools.

It reads files. It never connects to a cluster, and it never applies anything.
The patch is a diff you review and apply yourself.

**Why not `kubent` or `pluto`?** Those list deprecated APIs. This one also:

- generates the mechanical rewrites as a `git apply`-able diff, including the
  Ingress backend/path field moves and the `autoscaling/v2beta1` metric
  restructuring, not just the `apiVersion` string;
- separates *your* objects from the ones owned by a Helm release, an Argo CD
  application or an operator, because patching those in your repo is reverted on
  the next sync — the vendor has to ship the new API first;
- reports upgrade risks that are not API removals at all: a PodDisruptionBudget
  that makes a node drain impossible, a `selector: {}` whose meaning flips in
  `policy/v1`, scheduling pinned to retired `beta.kubernetes.io/*` node labels;
- prints the hop-by-hop sequence with managed-addon version floors for EKS, AKS
  and GKE, and a plain-English go / no-go with the blockers named.

## Sample report

[`docs/sample-report.html`](docs/sample-report.html) and
[`docs/sample-report.patch`](docs/sample-report.patch) are generated from
[`testdata/broken-cluster`](testdata/broken-cluster), a deliberately broken
1.24 manifest set, by `make sample`. The HTML is one self-contained file: no
JavaScript, no external CSS, no fonts, so it survives being an email attachment.

## Install

```sh
go install github.com/moveeeax/k8s-upgrade-report/cmd/k8s-upgrade-report@latest
```

Or from a clone:

```sh
git clone https://github.com/moveeeax/k8s-upgrade-report.git
cd k8s-upgrade-report
make build      # -> bin/k8s-upgrade-report
```

Requires Go 1.26 or newer. No other dependencies.

## Usage

The repository ships with a broken cluster you can run against straight away:

```sh
git clone https://github.com/moveeeax/k8s-upgrade-report.git
cd k8s-upgrade-report

go run ./cmd/k8s-upgrade-report scan \
  --manifests testdata/broken-cluster \
  --current 1.24 --target 1.29 --provider eks \
  --html report.html \
  --patch upgrade.patch
```

Real output, trimmed:

```text
k8s-upgrade-report dev  1.24 -> 1.29  (Amazon EKS)
source: testdata/broken-cluster

VERDICT: NO-GO
5 blocker(s) need a decision before the control plane moves: 3 owned by a
vendor chart or operator, 2 needing a hand-written change. 7 further object(s)
are mechanically fixable.

7 file(s), 12 object(s) scanned: 12 blocker(s), 2 warning(s), 3 vendor-owned, 7 auto-fixable

BLOCKERS (9)
  app/ingress.yaml:2  Ingress storefront
      Ingress extensions/v1beta1 is removed in 1.22
      fix: Rewrite to networking.k8s.io/v1. `k8s-upgrade-report patch` generates the
           diff.
  app/workloads.yaml:17  PodDisruptionBudget storefront
      PodDisruptionBudget makes node drains impossible
      fix: Relax the budget for the duration of the upgrade (maxUnavailable: 1) or
           delete it during the roll and restore it afterwards.
  platform/psp.yaml:1  PodSecurityPolicy restricted-legacy
      PodSecurityPolicy policy/v1beta1 is removed in 1.25
      fix: No replacement API exists - this needs a design decision, not a rewrite.

VENDOR MUST UPGRADE FIRST (3)
  charts/ingress-nginx/rendered.yaml:2  Ingress ingress-nginx-default-backend
      Ingress networking.k8s.io/v1beta1 is removed in 1.22
      owner: helm ingress-nginx (meta.helm.sh/release-name annotation)
      fix: Upgrade the chart behind ingress-nginx to a version that emits the new
           API, then re-render.

UPGRADE SEQUENCE
  1.24 -> 1.25: 5 affected object(s)
      batch/v1beta1 CronJob                          1 object(s)
      autoscaling/v2beta1 HorizontalPodAutoscaler    1 object(s)
      policy/v1beta1 PodDisruptionBudget             2 object(s)
      policy/v1beta1 PodSecurityPolicy               1 object(s)
      addon floors: aws-ebs-csi-driver >= 1.16.0, coredns >= 1.9.3, kube-proxy >= 1.25.6, vpc-cni >= 1.12.0
```

Check the generated patch before applying it — it is a normal diff:

```sh
git apply --check --directory=testdata/broken-cluster upgrade.patch
```

### Commands

| Command | What it does |
| --- | --- |
| `scan` | Report findings, sequencing and the go/no-go. `--format json` for machines, `--html FILE` for a client-ready page. |
| `patch` | Print only the diff, to stdout by default. |
| `rules` | Print the checked-in removal dataset; `--provider eks\|aks\|gke` prints that cloud's sequencing steps. |
| `version` | Print the binary version. |

### Flags

| Flag | Meaning |
| --- | --- |
| `--manifests` | Directory (or single file) of Kubernetes YAML to scan. |
| `--target` | Minor version you are upgrading to, e.g. `1.29`. Required. |
| `--current` | Minor version you run today. Defaults to one minor below `--target`. |
| `--provider` | `eks`, `aks` or `gke`. Adds addon floors and the node-pool ordering. |
| `--format` | `text` (default) or `json`. |
| `--html` | Also write a self-contained HTML report here. |
| `--patch` | Also write the generated diff here. |
| `--fail-on` | `none` (default), `warning` or `blocker`. Exit code 2 when the threshold is hit — use `blocker` in CI. |

Exit codes: `0` clean, `1` usage or I/O error, `2` findings at or above `--fail-on`.

## What it checks

`internal/rules/data/deprecations.yaml` is the whole dataset — plain YAML, one
entry per removed API, each with the version it was deprecated in, the version
it stops being served in, the replacement, whether the rewrite is mechanical,
and a link to the upstream deprecation guide. It covers the removals that
actually bite: Ingress (1.22), the RBAC and webhook beta APIs (1.22),
PodSecurityPolicy, CronJob, PodDisruptionBudget and HPA `v2beta1` (1.25), HPA
`v2beta2` and the flowcontrol betas (1.26 / 1.29 / 1.32), and the
ValidatingAdmissionPolicy beta.

`internal/rules/data/addons.yaml` holds the per-cloud addon floors and
sequencing steps. Both files are data: correcting a version is a one-line pull
request, and the tests fail if an entry is internally inconsistent.

Unrendered Helm templates are **skipped, not guessed at**, and listed in the
report as skipped so nobody mistakes an unscanned chart for a clean one. Render
the chart (`helm template`) into a directory and scan that instead.

## Scope

Deliberately not in this tool:

- No web app, no login, no hosted history. It is a binary that prints a file.
- No telemetry, no licence check, no network calls of any kind.
- No applying anything to a cluster. It generates a patch, you apply it.
- No CRD schema migration, operator upgrades or service-mesh version jumps.
- EKS, AKS and GKE only. No OpenShift, no kubeadm, no k3s.

Not built yet: reading live objects from a cluster via kubeconfig, with an RBAC
preflight so it fails loudly instead of under-reporting. Everything above works
against manifests on disk; the live-cluster path needs a real cluster to test
against and is the next slice, not a stub.

## Development

```sh
make test     # go test ./...
make lint     # gofmt + go vet
make sample   # regenerate docs/sample-report.html and docs/sample-report.patch
```

The patch generator is covered end to end: the test writes the fixtures into a
temporary git repository, applies the generated diff with `git apply`, re-scans
the result and asserts that nothing mechanically fixable is left and that the
rewritten Ingress really is a valid `networking.k8s.io/v1` object.

## License

[MIT](LICENSE)

---

Need someone to actually run this against your cluster and do the upgrade?
That is a fixed-price engagement — open an issue or reach me via
[github.com/moveeeax](https://github.com/moveeeax).
