{
  description = "Game Server Operator for Kubernetes — HostedGame CRD";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      nixpkgs,
      flake-utils,
      ...
    }:
    let
      # mkDeployment is parameterized over lib so the same module can be
      # evaluated against either nixpkgs.lib (used by hydraJobs / packages)
      # or lib from another flake (consumer-side composition in nixlab).
      mkDeployment = lib: import ./nix/game/deployment.nix { inherit lib; };

      # System-level outputs (per-system packages, devShells, checks).
      # hydraJobs at the top level reuses these.
      outputs = flake-utils.lib.eachDefaultSystem (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          lib = pkgs.lib;
          deployment = mkDeployment lib;

          # buildGoModule vendorHash: left as fakeHash deliberately. The
          # first `nix build .#game-operator` run will fail and print the
          # real hash; update it here and rebuild. This is the standard
          # bootstrapping pattern for Go flakes.
          game-operator = pkgs.buildGoModule {
            pname = "game-operator";
            version = deployment.chart.appVersion;
            src = ./.;
            # TODO(reconcile): replace with the real hash from the first
            # `nix build .#game-operator` run output.
            vendorHash = lib.fakeHash;
            subPackages = [
              "cmd/manager"
            ];
            ldflags = [
              "-s"
              "-w"
            ];
            # Tests need a kube API; skip during build.
            doCheck = false;
          };

          game-operator-image = pkgs.dockerTools.buildLayeredImage {
            name = deployment.image.repository;
            tag = "dev";
            contents = [ pkgs.cacert ];
            config = {
              Entrypoint = [ "${game-operator}/bin/manager" ];
              Env = [
                "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
              ];
              ExposedPorts = {
                "${toString deployment.operator.metricsPort}/tcp" = { };
              };
            };
          };
        in
        {
          packages = {
            default = game-operator;
            inherit game-operator game-operator-image;
            helm-chart = deployment.helmChart pkgs;
            k8s-manifests = deployment.k8sManifests pkgs;
          };

          checks = {
            inherit (outputs.packages.${system})
              helm-chart
              k8s-manifests
              ;
          };

          devShells.default = pkgs.mkShell {
            packages = [
              pkgs.go_1_26
              pkgs.kubectl
              pkgs.kubernetes-helm
              pkgs.yq-go
              pkgs.golangci-lint
            ];
          };
        }
      );
    in
    outputs
    // {
      # lib.game is exposed so other flakes (e.g. nixlab) can compose
      # against the same deployment definitions if they ever want to.
      lib.game = mkDeployment nixpkgs.lib;

      # hydraJobs: this is what gcp-hydra (hydra.casazza.io) builds.
      # Mirrors athena-operator's hydraJobs shape exactly.
      hydraJobs = {
        x86_64-linux = outputs.packages.x86_64-linux;
        aarch64-linux = outputs.packages.aarch64-linux;
      };

      formatter.x86_64-linux = nixpkgs.legacyPackages.x86_64-linux.nixfmt-rfc-style;
      formatter.aarch64-linux = nixpkgs.legacyPackages.aarch64-linux.nixfmt-rfc-style;
    };
}
