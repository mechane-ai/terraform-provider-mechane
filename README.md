# Mechane provider

The Mechane OpenTofu provider installs Workers and Cloudflare Containers from immutable releases. Use the
[Mechane installation module](https://github.com/mechane-ai/mechane) for the application, Console, and environments. The
official Cloudflare provider owns Worker identities, custom domains, and R2.

Set `CLOUDFLARE_API_TOKEN` in the process environment. The token needs Workers and Containers access, plus Secrets Store
Edit when binding account secrets. Credentials never become resource attributes.

The installation uses these resources:

- `mechane_cloudflare_worker_release` verifies qualification and all Worker modules and Console assets, uploads them,
  and exclusively activates a version with declared Durable Object exports and secret references. Removing it retains
  the Worker and data.
- `mechane_cloudflare_container_image` verifies a local qualified OCI archive, copies its Linux amd64 image into the
  account registry, and waits for Cloudflare runtime image preparation. The release supplies the archive SHA-256 and
  image digest; `source_file` names the prepared archive. Installation needs no source checkout, publication
  prerequisite, or Docker daemon.
- `mechane_cloudflare_machine_application` binds Durable Object-scheduled compute to MachineObject. The Worker owns
  image selection and native snapshots; the control-store ledger owns aggregate and workspace admission limits. The
  provider verifies application name, namespace and scheduling policy before updates or deletion.

`mechane_cloudflare_container_application` remains available to retire an existing default-scheduled application. It
cannot adopt or delete a Durable Object-scheduled application. Complete its checkpoint and retirement procedure before
switching compute ownership; retain the workspace namespace and bucket.

Import a Worker release with `<account_id>/<worker_name>` or append `/<version_id>` to select a version. Import an
existing application with `<account_id>/<application_id>`. Import an existing image with
`<account_id>/<repository>@sha256:<digest>`. Keep the corresponding release configuration in OpenTofu. Application
imports enable deletion protection. Apply `delete_protection = false` before destruction.

Worker releases require `release_manifest_file` and `release_manifest_sha256` for installation format 2, plus
`qualification_file` and `qualification_sha256`. The qualification receipt pins the same source and version, artifact
inventory, installation metadata, toolchain, fixtures, policy, configuration, and successful check evidence. All named
files remain available beside the receipt. A changed file fails before any Cloudflare request. The provider uploads the
exact inventoried bytes and runs no build command.

The installation supplies primary and additional module digests, Console asset paths and content types, compatibility
settings, Durable Object bindings and exports, and container classes. Supply only installation-specific text, bucket,
and secret-reference bindings. The release owns the ASSETS and Durable Object bindings.

Assets follow Cloudflare's
[direct upload protocol](https://developers.cloudflare.com/workers/static-assets/direct-upload/): register the manifest,
upload requested multipart base64 buckets with the session token, then attach the completion token to the Worker
version. Tokens remain transient. Missing completion never uploads or activates a version. All routes run the Worker
first, so Console authorization and routing remain application-owned.

Version readback checks uploaded module bytes, asset configuration, and the marker binding source, installation,
qualification, and normalized asset inventory digests. Cloudflare's version API does not return asset file bytes;
qualified upload and completion establish their transfer. Verify actual asset serving during hosted qualification.

Before uploading or activating a version, the provider verifies the live release's recorded manifest from public GitHub
release storage and preserves every wire and migration entry. Historical format 1 is read only for this guard. Additions
are allowed; removals, changed entries, and version downgrades are rejected. Manifest downloads have bounded deadlines
and sizes, and never use credentials. Unmarked Workers are not adopted. A new installation needs its own Worker
identity. The provider checks deployment identity again before a write and stops if it observes a concurrent change.
Cloudflare does not offer atomic compare-and-swap for deployment, so use one deployment writer per installation.

Every intentional release create or update requests
[`code_update_strategy: { "mode": "immediate" }`](https://developers.cloudflare.com/durable-objects/deployments/durable-objects-code-updates/)
with exclusive version activation. Active objects reset onto the new code, including every exported Durable Object
class, even when the selected version is already active. The setting belongs to each deployment and is write-only; the
provider requires a successful write acknowledgement and confirms its deployment identity and exclusive version. Traffic
readback cannot establish the adoption policy after a rejected or lost response. Atomic creation of a new bound
namespace uses `versions?deploy=true`, followed by an immediate deployment in the same operation under the observed
atomic deployment's identity guard. Verify actual actor behavior after deployment.

Deleting an application stops its machines. It never deletes the Worker, Durable Object namespace, or R2 files. Removing
an image resource retains its registry bytes because running machines and earlier releases can still reference them.
Registry garbage collection is a separate operator action.

Image create and update both require a matching `ready` result from Cloudflare's
[image preparation API](https://developers.cloudflare.com/api/resources/containers/subresources/images/methods/prepare/),
including when the registry already contains the manifest. Pending preparation shares the operation deadline; terminal
errors, mismatched images and incomplete ready responses fail the apply. The prepared artifact digest is separate from
the OCI image digest.

Image refresh only verifies registry content and starts no preparation. A no-change plan does not establish runtime
readiness for images installed by an older provider or imported from existing state. An explicitly reviewed image
resource replacement resumes installation and preparation while retaining the immutable registry bytes.

An apply verifies accepted application identities and settings. Durable Object-scheduled applications have no image
rollout; image and configuration changes require retiring affected machines before activating their new owner. Verify a
cold sandbox through Mechane after installation to prove actual startup and file restoration.

The provider version is declared in [release.json](release.json) and advances independently of Mechane service releases.
The installation module pins the provider version it requires.

The schema 0 to 1 state upgrade retains image, Worker, version, deployment, and namespace identities. It removes the old
image download URL and leaves the new candidate path and qualification fields empty until installation configuration
supplies them. The upgrade makes no cloud request and invents no qualification evidence.

Build with Go 1.27.3:

```sh
mise exec -- go build ./...
mise exec -- go vet ./...
mise exec -- go test ./...
```

The implementation uses Cloudflare's public Containers API and the Terraform plugin framework. It invokes no deployment
CLI or shell provisioner.
