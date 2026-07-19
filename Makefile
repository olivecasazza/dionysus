.PHONY: manifests build test nix-chart nix-image nix-manifests nix-fmt

# Generate CRDs from Go API types via controller-gen. Run after editing
# api/v1alpha1/*.go and commit the result. The helm-chart derivation
# bundles the committed CRDs verbatim.
manifests:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0 \
		paths=./api/... \
		crd:crdVersions=v1 \
		output:dir=./charts/dionysus/crds
	@mv charts/dionysus/crds/games.dionysus.io_hostedgames.yaml charts/dionysus/crds/games-crds.yaml || true

build:
	go build ./...

test:
	go test ./...

nix-chart:
	nix build .#helm-chart --print-out-paths

nix-image:
	nix build .#dionysus-image --print-out-paths

nix-manifests:
	nix build .#k8s-manifests --print-out-paths

nix-fmt:
	nix fmt
