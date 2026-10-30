# Mechane provider

The Mechane OpenTofu provider installs Workers and Cloudflare Containers from immutable releases. Use the
[Mechane installation module](https://github.com/mechane-ai/mechane) for a complete service. The official Cloudflare
provider owns Worker identities, custom domains, and R2.

Set `CLOUDFLARE_API_TOKEN` in the process environment. The token needs Workers and Containers access, plus Secrets Store
Edit when binding account secrets. Credentials never become resource attributes.

The provider supplies three building blocks:

- `mechane_cloudflare_worker_release` verifies the Worker bundle and activates its version with declarative Durable
  Object exports and secret references. Removing it retains the Worker and data.
- `mechane_cloudflare_container_image` verifies a public OCI archive and copies its Linux amd64 image into the account
  registry. The release supplies the archive URL, archive SHA-256, and image digest. Installation needs no source
  checkout or Docker daemon.
- `mechane_cloudflare_container_application` binds a default-scheduled application to an existing Durable Object
  namespace. It manages capacity and image rollouts, reads remote drift, and waits for active rollouts before continuing
  an interrupted update.

Import a Worker release with `<account_id>/<worker_name>` or append `/<version_id>` to select a version. Import an
existing application with `<account_id>/<application_id>`. Import an existing image with
`<account_id>/<repository>@sha256:<digest>`. Keep the corresponding release configuration in OpenTofu. Application
imports enable deletion protection. Apply `delete_protection = false` before destruction.

Worker releases require `release_manifest_file` and `release_manifest_sha256` for the published `installation.json`.
Before uploading or activating a version, the provider verifies the live release's recorded manifest from public GitHub
release storage and preserves every wire and migration entry. Additions are allowed; removals, changed entries, and
version downgrades are rejected. Manifest downloads have bounded deadlines and sizes, and never use credentials. An
unmarked legacy Worker requires a reviewed `adoption_baseline` JSON string containing `worker_version_id`,
`bundle_sha256`, `release_version`, and `formats` with complete `wire` and `migrations` maps. Its ID and digest must
match the active Worker. Once adopted, the recorded native manifest is authoritative; remove the baseline input without
uploading another version. A baseline cannot bypass a missing or invalid native manifest. The provider checks deployment
identity again before a write and stops if it observes a concurrent change. Cloudflare does not offer atomic
compare-and-swap for deployment, so use one deployment writer per installation.

Deleting an application stops its machines. It never deletes the Worker, Durable Object namespace, or R2 files. Removing
an image resource retains its registry bytes because running machines and earlier releases can still reference them.
Registry garbage collection is a separate operator action.

An apply waits for Cloudflare's deployment configuration to converge and rejects an explicit unhealthy target, even
after its rollout ends. A sleeping application can have no running instances, and Cloudflare does not provide a health
summary for every rollout strategy. Verify a cold sandbox through Mechane after installation to prove actual startup.
Worker and guest releases must tolerate the mixed-version interval during a rollout.

Build with Go 1.27.3:

```sh
go mod download
go build ./...
go vet ./...
```

The implementation uses Cloudflare's public Containers API and the Terraform plugin framework. It invokes no deployment
CLI or shell provisioner.
