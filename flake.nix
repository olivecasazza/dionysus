{
  description = "Game Server Operator for Kubernetes — HostedGame CRD";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    hydra-src = {
      url = "github:olivecasazza/hydra/4060fb888f85b3c273ac56cae665e7c99e99fe1c";
      flake = false;
    };
  };

  outputs =
    {
      nixpkgs,
      flake-utils,
      hydra-src,
      ...
    }:
    let
      # mkDeployment is parameterized over lib so the same module can be
      # evaluated against either nixpkgs.lib (used by hydraJobs / packages)
      # or lib from another flake (consumer-side composition in nixlab).
      mkDeployment = lib: import ./nix/dionysus/deployment.nix { inherit lib; };
      systems = flake-utils.lib.defaultSystems;
      ci = import "${hydra-src}/lib/ci.nix" {
        inherit (nixpkgs) lib;
      };

      buildFor =
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          lib = pkgs.lib;
          deployment = mkDeployment lib;

          # buildGoModule vendorHash: left as fakeHash deliberately. The
          # first `nix build .#dionysus` run will fail and print the
          # real hash; update it here and rebuild. This is the standard
          # bootstrapping pattern for Go flakes.
          dionysus = pkgs.buildGoModule {
            pname = "dionysus-operator";
            version = deployment.chart.appVersion;
            src = ./.;
            # TODO(reconcile): replace with the real hash from the first
            # `nix build .#dionysus` run output.
            vendorHash = "sha256-sD4JtEFay+CugYvV1gQRKcfqNqxkQ9ATOw1VXTm+4aE=";
            subPackages = [
              "cmd/manager"
              "cmd/discord-bot"
            ];
            ldflags = [
              "-s"
              "-w"
            ];
            # Tests need a kube API; skip during build.
            doCheck = false;
          };

          dionysus-image = pkgs.dockerTools.buildLayeredImage {
            name = deployment.image.repository;
            tag = "dev";
            contents = [ pkgs.cacert ];
            config = {
              Entrypoint = [ "${dionysus}/bin/manager" ];
              Env = [
                "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
              ];
              ExposedPorts = {
                "${toString deployment.operator.metricsPort}/tcp" = { };
              };
            };
          };

          # discord-image is the HTTP interactions bot. Runs in its own
          # Deployment; see deployment.nix k8sObjects. The image is
          # published to the same GHCR path under a :discord tag.
          dionysus-discord-image = pkgs.dockerTools.buildLayeredImage {
            name = "ghcr.io/olivecasazza/dionysus-discord";
            tag = "dev";
            contents = [ pkgs.cacert ];
            config = {
              Entrypoint = [ "${dionysus}/bin/discord-bot" ];
              Env = [
                "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
              ];
              ExposedPorts = {
                "8080/tcp" = { };
              };
            };
          };

          packages = {
            default = dionysus;
            inherit dionysus dionysus-image dionysus-discord-image;
            helm-chart = deployment.helmChart pkgs;
            k8s-manifests = deployment.k8sManifests pkgs;
          };

          checks = {
            inherit (packages) helm-chart k8s-manifests;
          };
        in
        {
          inherit packages checks;
          devShells.default = pkgs.mkShell {
            packages = [
              pkgs.go_1_26
              pkgs.kubectl
              pkgs.kubernetes-helm
              pkgs.yq-go
              pkgs.golangci-lint
            ];
          };
        };

      perSystem = nixpkgs.lib.genAttrs systems buildFor;
      ciOutputs = ci.mkOutputs {
        inherit systems;
        packageSystems = systems;
        checkSystems = [ "x86_64-linux" ];
        # Hydra's deployed machine pool is x86_64-linux; package systems
        # remain multi-system for native consumers.
        hydraSystems = [ "x86_64-linux" ];
        buildFor = system: perSystem.${system};
      };
    in
    ciOutputs
    // {
      devShells = nixpkgs.lib.genAttrs systems (system: perSystem.${system}.devShells);

      # lib.game is exposed so other flakes (e.g. nixlab) can compose
      # against the same deployment definitions if they ever want to.
      lib.game = mkDeployment nixpkgs.lib;

      formatter.x86_64-linux = nixpkgs.legacyPackages.x86_64-linux.nixfmt-rfc-style;
      formatter.aarch64-linux = nixpkgs.legacyPackages.aarch64-linux.nixfmt-rfc-style;
    };
}
