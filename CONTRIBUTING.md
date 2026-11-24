

# Contributing to antrea-operator-for-kubernetes

The antrea-operator-for-kubernetes project team welcomes contributions from the community. If you wish to contribute code and you have not signed our contributor license agreement (CLA), our bot will update the issue when you open a Pull Request. For any questions about the CLA process, please refer to our [FAQ](https://cla.vmware.com/faq).

## Contribution Flow

This is a rough outline of what a contributor's workflow looks like:

- Create a topic branch from where you want to base your work
- Make commits of logical units
- Make sure your commit messages are in the proper format (see below)
- Push your changes to a topic branch in your fork of the repository
- Submit a pull request

Example:

``` shell
git remote add upstream https://github.com/antrea-io/antrea-operator.git
git checkout -b my-new-feature main
git commit -a
git push origin my-new-feature
```

### Staying In Sync With Upstream

When your branch gets out of sync with the vmware/main branch, use the following to update:

``` shell
git checkout my-new-feature
git fetch -a
git pull --rebase upstream main
git push --force-with-lease origin my-new-feature
```

### Updating pull requests

If your PR fails to pass CI or needs changes based on code review, you'll most likely want to squash these changes into
existing commits.

If your pull request contains a single commit or your changes are related to the most recent commit, you can simply
amend the commit.

``` shell
git add .
git commit --amend
git push --force-with-lease origin my-new-feature
```

If you need to squash changes into an earlier commit, you can use:

``` shell
git add .
git commit --fixup <commit>
git rebase -i --autosquash main
git push --force-with-lease origin my-new-feature
```

Be sure to add a comment to the PR indicating your new changes are ready to review, as GitHub does not generate a
notification when you git push.

### Code Style

### Formatting Commit Messages

We follow the conventions on [How to Write a Git Commit Message](http://chris.beams.io/posts/git-commit/).

Be sure to include any related GitHub issue references in the commit message.  See
[GFM syntax](https://guides.github.com/features/mastering-markdown/#GitHub-flavored-markdown) for referencing issues
and commits.

## Maintaining the Go module

The public module path is `antrea.io/antrea-operator`. Bring changes to
the operator maintainers through a pull request to this repository. An
initial reviewer for the module handoff is Antonin Bas (`@antoninbas`),
who merged the module rename. Ask the Antrea maintainers to confirm a
primary and backup reviewer for dependencies, generators, and exported
controller interfaces. Review ownership and interface acceptance are
pending until those maintainers acknowledge them.
[Module rename](https://github.com/antrea-io/antrea-operator/pull/1) ·
[Antrea maintainers](https://github.com/antrea-io/antrea/blob/main/MAINTAINERS.md)

Update the OpenShift dependencies together. The current source is Cluster
Network Operator (CNO) release-4.22 commit `c3e2e335a4d5`. It uses
Kubernetes API and client-go v0.35.2, controller-runtime v0.23.1, and
requires **apimachinery v0.36.0-alpha.2**. Go module selection propagates
that requirement to consumers. Check the selected versions in the final
consumer graph whenever either project changes its pin. Keep dependency
requirements in `go.mod` usable by an importing module, since Go ignores
replace directives in dependency modules.
[CNO go.mod](https://github.com/openshift/cluster-network-operator/blob/c3e2e335a4d52f4e5f97e35a9428305bd8075a94/go.mod)

For a dependency or API change, run these checks from the repository root
with Go 1.25 or later. Inspect the generated CRD for field, validation,
and status-subresource changes. Run generation twice to check for drift.

```sh
make generate crds
make test-tidy test golangci
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
```

`make crds` generates the public CRD. `make manifests` also rebuilds
the operand resources selected by `VERSION` with Python 3. When PyYAML is missing,
the generator installs PyYAML 6.0.3 in a temporary virtual environment. The checked-in operand
version and live platform qualification are separate release inputs.

A downstream integrator can build against a checkout with a temporary
local replacement while a change is in review. For release, wait for
acceptance upstream, then resolve the merged commit with
`go get antrea.io/antrea-operator@<merged-commit>` in the consuming
module. Record the resolved module version and checksum. For the
Antrea 2.8 composition, review `go list -m all` and run the downstream
controller, CRD compatibility, and Linux amd64 build checks with its
actual source pins. The standalone module's tests cover a different
dependency graph.

## Reporting Bugs and Creating Issues

When opening a new issue, try to roughly follow the commit message format conventions above.
