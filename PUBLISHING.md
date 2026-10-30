# Provider releases

This repository contains public source snapshots published by the `mechane-engineering` GitHub App. Both commit
identities and release tags belong to that App. Each release builds from its public source commit.

Release archives contain the provider binary, this project's Apache license and notice, and dependency licenses. The
signed SHA-256 checksum file covers every platform archive and the registry manifest. The public signing key is included
as a separate release asset. Published versions are immutable.

The OpenTofu registry requires a one-time provider and signing-key registration through its
[GitHub issue forms](https://github.com/opentofu/registry#adding-providers-modules-or-gpg-keys-to-the-opentofu-registry).
The signing-key submitter must have publicly visible membership in the publishing organization. The registry indexes
subsequent releases automatically after registration is accepted.

Consumers install the provider through `tofu init`. Individual Mechane agents and conversations remain application data
managed through the Mechane API, SDK or CLI.
