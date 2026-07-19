{ lib }:

# deployment.nix is the load-bearing packaging file. It renders both the
# Helm chart (templates/*.yaml, values.yaml, Chart.yaml) and the raw k8s
# manifests from pure Nix data, exactly mirroring the athena-operator
# pattern at /home/olive/Repositories/athena-operator/nix/athena/deployment.nix.
#
# Flux consumes the Helm chart via GitRepository + HelmRelease (see
# nixlab/modules/k8s/apps/dionysus.nix). Hydra builds the chart and
# the OCI image (see flake.nix hydraJobs).
let
  inherit (lib) filterAttrs;
in
rec {
  chart = {
    name = "dionysus";
    description = "Dionysus — Game Server Operator for Kubernetes (HostedGame CRD)";
    version = "0.1.0";
    appVersion = "0.1.0";
  };

  namespace = "games";
  releaseName = "dionysus";

  image = {
    repository = "ghcr.io/olivecasazza/dionysus";
    pullPolicy = "IfNotPresent";
    # Flux ImagePolicy replaces this with the latest digest-pinned tag.
    # The literal "latest" is a placeholder; the nixlab HelmRelease
    # carries the {"$imagepolicy": "apps:dionysus"} setter marker.
    tag = "latest";
  };

  operator = {
    replicas = 1;
    metricsPort = 8080;
    serviceAccountName = "dionysus";
    resources = {
      requests = {
        cpu = "100m";
        memory = "128Mi";
      };
      limits = {
        memory = "256Mi";
      };
    };
  };

  observability = {
    metrics = {
      enabled = true;
      serviceMonitor = {
        enabled = true;
        interval = "15s";
      };
    };
  };

  api = {
    group = "games.dionysus.io";
    version = "v1alpha1";
    resources = [ "hostedgames" ];
    statusResources = map (resource: "${resource}/status") api.resources;
    finalizerResources = map (resource: "${resource}/finalizers") api.resources;
  };

  # ClusterRole rules granted to the operator ServiceAccount. The
  # operator owns HostedGame CRs and manages their workload children
  # (Deployments, Services, PVCs) plus backup CronJobs/Jobs. pods/exec
  # is needed for the lifecycle PreSaveCommand / StopCommand hooks.
  operatorRules = [
    {
      apiGroups = [ api.group ];
      resources = api.resources;
      verbs = [
        "get"
        "list"
        "watch"
        "create"
        "update"
        "patch"
        "delete"
      ];
    }
    {
      apiGroups = [ api.group ];
      resources = api.statusResources ++ api.finalizerResources;
      verbs = [
        "get"
        "update"
        "patch"
      ];
    }
    {
      apiGroups = [ "apps" ];
      resources = [ "deployments" ];
      verbs = [
        "get"
        "list"
        "watch"
        "create"
        "update"
        "patch"
        "delete"
      ];
    }
    {
      # core/v1 resources the controller renders or observes.
      # services: per-game ClusterIP.
      # persistentvolumeclaims: per-game PVCs.
      # pods, pods/log: lifecycle exec + status observation.
      # pods/exec: PreSaveCommand / StopCommand hooks.
      # events: emit reconcile events.
      # secrets: read S3 credentials (mounted into backup CronJob pods).
      apiGroups = [ "" ];
      resources = [
        "services"
        "persistentvolumeclaims"
        "pods"
        "pods/log"
        "pods/exec"
        "events"
        "secrets"
      ];
      verbs = [
        "get"
        "list"
        "watch"
        "create"
        "update"
        "patch"
        "delete"
      ];
    }
    {
      # configmaps: lazymc proxy config injection (future) + status
      # scratch for idle-scaled games.
      apiGroups = [ "" ];
      resources = [ "configmaps" ];
      verbs = [
        "get"
        "list"
        "watch"
        "create"
        "update"
        "patch"
        "delete"
      ];
    }
    {
      # batch: per-game backup CronJobs + ad-hoc backup-now Jobs.
      apiGroups = [ "batch" ];
      resources = [
        "cronjobs"
        "jobs"
      ];
      verbs = [
        "get"
        "list"
        "watch"
        "create"
        "update"
        "patch"
        "delete"
      ];
    }
  ];

  labels = {
    "app.kubernetes.io/name" = chart.name;
    "app.kubernetes.io/instance" = releaseName;
    "app.kubernetes.io/version" = chart.appVersion;
    "app.kubernetes.io/managed-by" = "Helm";
    "helm.sh/chart" = "${chart.name}-${chart.version}";
  };

  selectorLabels = filterAttrs (
    name: _value:
    builtins.elem name [
      "app.kubernetes.io/name"
      "app.kubernetes.io/instance"
    ]
  ) labels;

  fullname = releaseName;

  k8sObjects = [
    # ServiceAccount used by the operator Deployment.
    {
      apiVersion = "v1";
      kind = "ServiceAccount";
      metadata = {
        name = operator.serviceAccountName;
        inherit labels;
      };
    }

    # Operator Deployment. Single replica (leader-elect off by default).
    {
      apiVersion = "apps/v1";
      kind = "Deployment";
      metadata = {
        name = fullname;
        inherit labels;
      };
      spec = {
        replicas = operator.replicas;
        selector.matchLabels = selectorLabels;
        template = {
          metadata.labels = selectorLabels;
          spec = {
            serviceAccountName = operator.serviceAccountName;
            imagePullSecrets = [ { name = "ghcr-pull"; } ];
            containers = [
              {
                name = chart.name;
                image = "${image.repository}:${image.tag}";
                imagePullPolicy = image.pullPolicy;
                env = [
                  {
                    name = "METRICS_PORT";
                    value = toString operator.metricsPort;
                  }
                ];
                ports = [
                  {
                    name = "metrics";
                    containerPort = operator.metricsPort;
                    protocol = "TCP";
                  }
                ];
                resources = operator.resources;
                # healthz / readyz are wired by the manager (cmd/manager/main.go).
                livenessProbe.httpGet = {
                  path = "/healthz";
                  port = "probe";
                };
                readinessProbe.httpGet = {
                  path = "/readyz";
                  port = "probe";
                };
              }
            ];
          };
        };
      };
    }

    # Metrics Service: ClusterIP exposing the metrics port for the
    # ServiceMonitor to scrape.
    {
      apiVersion = "v1";
      kind = "Service";
      metadata = {
        name = "${fullname}-metrics";
        inherit labels;
      };
      spec = {
        type = "ClusterIP";
        ports = [
          {
            port = operator.metricsPort;
            targetPort = "metrics";
            protocol = "TCP";
            name = "metrics";
          }
        ];
        selector = selectorLabels;
      };
    }

    # ClusterRole + Binding granting the operator the rules above.
    {
      apiVersion = "rbac.authorization.k8s.io/v1";
      kind = "ClusterRole";
      metadata = {
        name = "${fullname}-role";
        inherit labels;
      };
      rules = operatorRules;
    }
    {
      apiVersion = "rbac.authorization.k8s.io/v1";
      kind = "ClusterRoleBinding";
      metadata = {
        name = "${fullname}-binding";
        inherit labels;
      };
      roleRef = {
        apiGroup = "rbac.authorization.k8s.io";
        kind = "ClusterRole";
        name = "${fullname}-role";
      };
      subjects = [
        {
          kind = "ServiceAccount";
          name = operator.serviceAccountName;
          namespace = "{{ .Release.Namespace }}";
        }
      ];
    }

    # ServiceMonitor for Prometheus scraping. Conditional on
    # observability.metrics.serviceMonitor.enabled.
    {
      apiVersion = "monitoring.coreos.com/v1";
      kind = "ServiceMonitor";
      metadata = {
        name = fullname;
        inherit labels;
      };
      spec = {
        selector.matchLabels = selectorLabels;
        endpoints = [
          {
            port = "metrics";
            interval = observability.metrics.serviceMonitor.interval;
          }
        ];
      };
    }
  ];

  helmValues = {
    replicaCount = operator.replicas;
    image = image // {
      tag = "";
    };
    imagePullSecrets = [ { name = "ghcr-pull"; } ];
    serviceAccount = {
      create = true;
      name = operator.serviceAccountName;
    };
    metrics = {
      enabled = observability.metrics.enabled;
      port = operator.metricsPort;
      serviceMonitor = observability.metrics.serviceMonitor;
    };
    resources = operator.resources;
  };

  # ── Helm chart rendering ──────────────────────────────────────────
  #
  # Same pattern as athena-operator: a literal Deployment object is
  # derived from k8sObjects, then we substitute the image + namespace
  # fields with Helm template syntax so chart consumers can override
  # them via values.

  operatorDeployment = builtins.elemAt k8sObjects 1;
  operatorContainer = builtins.elemAt operatorDeployment.spec.template.spec.containers 0;
  operatorClusterRoleBinding = builtins.elemAt k8sObjects 4;

  helmDeployment = lib.recursiveUpdate operatorDeployment {
    spec.template.spec.containers = [
      (
        operatorContainer
        // {
          image = "{{ .Values.image.repository }}:{{ .Values.image.tag }}";
          imagePullPolicy = "{{ .Values.image.pullPolicy }}";
        }
      )
    ];
  };

  helmOperatorClusterRoleBinding = lib.recursiveUpdate operatorClusterRoleBinding {
    subjects = [
      {
        kind = "ServiceAccount";
        name = operator.serviceAccountName;
        namespace = "{{ .Release.Namespace }}";
      }
    ];
  };

  renderYaml =
    pkgs: name: value:
    (pkgs.formats.yaml { }).generate name value;

  renderObjects =
    pkgs: name: objects:
    pkgs.runCommand name { nativeBuildInputs = [ pkgs.yq-go ]; } ''
      cp ${renderYaml pkgs "objects.yaml" { items = objects; }} objects.yaml
      yq eval '.items[] | splitDoc' objects.yaml > $out
    '';

  removeNulls =
    value:
    if builtins.isAttrs value then
      filterAttrs (_: v: v != null) (builtins.mapAttrs (_: removeNulls) value)
    else if builtins.isList value then
      map removeNulls value
    else
      value;

  helmTemplates = pkgs: {
    deployment = renderObjects pkgs "deployment.yaml" [ helmDeployment ];
    service = renderObjects pkgs "service.yaml" [
      (builtins.elemAt k8sObjects 2)
    ];
    observability = renderObjects pkgs "observability.yaml" [
      (builtins.elemAt k8sObjects 5)
    ];
    rbac = renderObjects pkgs "rbac.yaml" [
      (builtins.elemAt k8sObjects 0)
      (builtins.elemAt k8sObjects 3)
      helmOperatorClusterRoleBinding
    ];
  };

  # helmChart assembles the chart directory consumed by Flux's
  # HelmRelease. CRDs come from the committed charts/dionysus/crds/ dir
  # (generated from Go types via controller-gen); templates come from
  # the rendered Helm-syntax objects above.
  helmChart =
    pkgs:
    let
      templates = helmTemplates pkgs;
    in
    pkgs.stdenvNoCC.mkDerivation {
      pname = "dionysus-helm-chart";
      version = chart.version;
      src = ../../charts/dionysus;
      dontBuild = true;
      installPhase = ''
        mkdir -p $out/templates $out/crds
        cp -r crds/. $out/crds/
        cp ${templates.deployment} $out/templates/deployment.yaml
        cp ${templates.service} $out/templates/service.yaml
        cp ${templates.observability} $out/templates/observability.yaml
        cp ${templates.rbac} $out/templates/rbac.yaml
        cp ${renderYaml pkgs "values.yaml" (removeNulls helmValues)} $out/values.yaml
        cp ${
          renderYaml pkgs "Chart.yaml" {
            apiVersion = "v2";
            inherit (chart)
              name
              description
              version
              appVersion
              ;
            type = "application";
          }
        } $out/Chart.yaml
      '';
    };

  # k8sManifests emits a single merged manifest file suitable for
  # `kubectl apply -f` or for inspection / diffing. Flux uses the chart
  # (above) in production; this derivation is used by CI checks and by
  # the pre-commit generate-k8s-manifests hook in nixlab.
  k8sManifests =
    pkgs:
    pkgs.stdenvNoCC.mkDerivation {
      pname = "dionysus-k8s-manifests";
      version = chart.version;
      dontUnpack = true;
      nativeBuildInputs = [ pkgs.yq-go ];
      installPhase = ''
        cat ${renderObjects pkgs "game-manifests.yaml" k8sObjects} > $out
      '';
    };
}
