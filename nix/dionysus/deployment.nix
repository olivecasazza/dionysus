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
    name = "dionysus-operator";
    description = "Dionysus — Game Server Operator for Kubernetes (HostedGame CRD)";
    version = "0.1.0";
    appVersion = "0.1.0";
  };

  namespace = "games";
  releaseName = "dionysus-operator";

  image = {
    repository = "ghcr.io/olivecasazza/dionysus-operator";
    pullPolicy = "IfNotPresent";
    # Flux ImagePolicy replaces this with the latest digest-pinned tag.
    # The literal "latest" is a placeholder; the nixlab HelmRelease
    # carries the {"$imagepolicy": "apps:dionysus-operator"} setter marker.
    tag = "latest";
  };

  operator = {
    replicas = 1;
    metricsPort = 8080;
    serviceAccountName = "dionysus-operator";
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
    # Colocated Grafana dashboard shipped by the chart. Auto-integrates with
    # the cluster Grafana via the operator's dashboards=grafana instanceSelector;
    # lands in its own folder. Gated by .Values.grafana.enabled so consumers
    # without the Grafana operator can turn it off.
    grafana = {
      enabled = true;
      folder = "Dionysus";
    };
  };

  # discord is the HTTP interactions bot. Runs as its own Deployment
  # alongside the controller. Disabled by default — operator deploys
  # without it; nixlab opts in via HelmRelease values when the Discord
  # app credentials exist.
  discord = {
    enabled = false;
    port = 8080;
    image = {
      # Keep the discord image name fixed so it stays dionysus-discord even
      # though the operator image was renamed to dionysus-operator.
      repository = "ghcr.io/olivecasazza/dionysus-discord";
      pullPolicy = "IfNotPresent";
      tag = "latest";
    };
    resources = {
      requests = {
        cpu = "50m";
        memory = "64Mi";
      };
      limits = {
        memory = "128Mi";
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
                  {
                    name = "probe";
                    containerPort = 8081;
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

    # ── Discord bot ────────────────────────────────────────────────
    # The bot is a separate Deployment+Service so its rollout, scaling,
    # and resource profile are independent from the controller. Disabled
    # by default; enabled via HelmRelease values when Discord app
    # credentials exist. The image is the same Go module's cmd/discord-bot
    # binary, published under a -discord tag suffix.

    # Discord Service: ClusterIP on the bot's HTTP port. An Ingress
    # (created outside the operator chart, since it depends on the
    # cluster's ingress setup) exposes /interactions to Discord.
    {
      apiVersion = "v1";
      kind = "Service";
      metadata = {
        name = "${fullname}-discord";
        labels = labels // {
          "app.kubernetes.io/component" = "discord";
        };
      };
      spec = {
        type = "ClusterIP";
        ports = [
          {
            port = discord.port;
            targetPort = "http";
            protocol = "TCP";
            name = "http";
          }
        ];
        selector = selectorLabels // {
          "app.kubernetes.io/component" = "discord";
        };
      };
    }

    # Discord Deployment. Env vars come from a Secret named
    # <release>-discord-creds (created by the consumer; the operator
    # chart does not own the Secret so consumers can rotate without a
    # chart bump).
    {
      apiVersion = "apps/v1";
      kind = "Deployment";
      metadata = {
        name = "${fullname}-discord";
        labels = labels // {
          "app.kubernetes.io/component" = "discord";
        };
      };
      spec = {
        replicas = 1;
        selector.matchLabels = selectorLabels // {
          "app.kubernetes.io/component" = "discord";
        };
        template = {
          metadata.labels = selectorLabels // {
            "app.kubernetes.io/component" = "discord";
          };
          spec = {
            serviceAccountName = operator.serviceAccountName;
            imagePullSecrets = [ { name = "ghcr-pull"; } ];
            containers = [
              {
                name = "discord";
                image = "${discord.image.repository}:${discord.image.tag}";
                imagePullPolicy = discord.image.pullPolicy;
                ports = [
                  {
                    name = "http";
                    containerPort = discord.port;
                    protocol = "TCP";
                  }
                ];
                env = [
                  {
                    name = "DISCORD_PUBLIC_KEY";
                    valueFrom.secretKeyRef = {
                      name = "${fullname}-discord-creds";
                      key = "public-key";
                    };
                  }
                  {
                    name = "DISCORD_APP_ID";
                    valueFrom.secretKeyRef = {
                      name = "${fullname}-discord-creds";
                      key = "app-id";
                    };
                  }
                  {
                    name = "DISCORD_BOT_TOKEN";
                    valueFrom.secretKeyRef = {
                      name = "${fullname}-discord-creds";
                      key = "bot-token";
                    };
                  }
                ];
                resources = discord.resources;
                livenessProbe.httpGet = {
                  path = "/healthz";
                  port = "http";
                };
                readinessProbe.httpGet = {
                  path = "/healthz";
                  port = "http";
                };
              }
            ];
          };
        };
      };
    }

    # ── Grafana dashboard (colocated observability) ────────────────
    # Ships with the chart so operator metrics land in Grafana automatically.
    # Rendered under a .Values.grafana.enabled conditional; carries the
    # dashboards=grafana instanceSelector the Grafana operator watches and a
    # dedicated folder, mirroring athena's dashboard organization. Panels use
    # only metrics that exist: the custom dionysus_hostedgame_* gauges plus
    # controller-runtime defaults.
    {
      apiVersion = "grafana.integreatly.org/v1beta1";
      kind = "GrafanaDashboard";
      metadata = {
        name = fullname;
        labels = labels // {
          app = "grafana";
        };
      };
      spec = {
        instanceSelector.matchLabels.dashboards = "grafana";
        folder = observability.grafana.folder;
        datasources = [
          {
            inputName = "DS_PROMETHEUS";
            datasourceName = "Prometheus";
          }
        ];
        json = builtins.toJSON {
          annotations.list = [ ];
          editable = true;
          graphTooltip = 1;
          schemaVersion = 39;
          style = "dark";
          tags = [
            "dionysus"
            "games"
            "operator"
          ];
          time = {
            from = "now-6h";
            to = "now";
          };
          title = "Dionysus / Operator";
          uid = "dionysus-operator";
          templating.list = [
            {
              name = "game";
              label = "Game";
              type = "query";
              datasource.uid = "prometheus";
              query = "label_values(dionysus_hostedgame_phase, game)";
              refresh = 2;
              includeAll = true;
              allValue = ".*";
              multi = true;
              current = {
                text = "All";
                value = "$__all";
              };
            }
          ];
          panels = [
            {
              type = "row";
              title = "HostedGames";
              gridPos = {
                h = 1;
                w = 24;
                x = 0;
                y = 0;
              };
              collapsed = false;
            }
            {
              type = "stat";
              title = "Games Running";
              datasource.uid = "prometheus";
              gridPos = {
                h = 6;
                w = 6;
                x = 0;
                y = 1;
              };
              targets = [
                {
                  datasource.uid = "prometheus";
                  expr = ''count(dionysus_hostedgame_phase{phase="Running"} == 1) or vector(0)'';
                  refId = "A";
                }
              ];
              fieldConfig.defaults.color.mode = "fixed";
            }
            {
              type = "timeseries";
              title = "Players Online (per game)";
              datasource.uid = "prometheus";
              gridPos = {
                h = 6;
                w = 9;
                x = 6;
                y = 1;
              };
              targets = [
                {
                  datasource.uid = "prometheus";
                  expr = ''dionysus_hostedgame_players_online{game=~"$game"}'';
                  legendFormat = "{{game}}";
                  refId = "A";
                }
              ];
              fieldConfig.defaults.color.mode = "palette-classic";
            }
            {
              type = "state-timeline";
              title = "Game Phase";
              datasource.uid = "prometheus";
              gridPos = {
                h = 6;
                w = 9;
                x = 15;
                y = 1;
              };
              targets = [
                {
                  datasource.uid = "prometheus";
                  expr = ''dionysus_hostedgame_phase{game=~"$game"} == 1'';
                  legendFormat = "{{game}} {{phase}}";
                  refId = "A";
                }
              ];
              fieldConfig.defaults.color.mode = "palette-classic";
            }
            {
              type = "row";
              title = "Operator Health";
              gridPos = {
                h = 1;
                w = 24;
                x = 0;
                y = 7;
              };
              collapsed = false;
            }
            {
              type = "timeseries";
              title = "Reconcile Rate";
              datasource.uid = "prometheus";
              gridPos = {
                h = 7;
                w = 8;
                x = 0;
                y = 8;
              };
              targets = [
                {
                  datasource.uid = "prometheus";
                  expr = ''sum by (controller) (rate(controller_runtime_reconcile_total{controller=~"hostedgame.*"}[5m]))'';
                  legendFormat = "{{controller}}";
                  refId = "A";
                }
              ];
              fieldConfig.defaults = {
                unit = "ops";
                color.mode = "palette-classic";
              };
            }
            {
              type = "timeseries";
              title = "Reconcile Errors";
              datasource.uid = "prometheus";
              gridPos = {
                h = 7;
                w = 8;
                x = 8;
                y = 8;
              };
              targets = [
                {
                  datasource.uid = "prometheus";
                  expr = ''sum by (controller) (rate(controller_runtime_reconcile_errors_total{controller=~"hostedgame.*"}[5m]))'';
                  legendFormat = "{{controller}}";
                  refId = "A";
                }
              ];
              fieldConfig.defaults = {
                unit = "ops";
                color = {
                  mode = "fixed";
                  fixedColor = "red";
                };
              };
            }
            {
              type = "timeseries";
              title = "Reconcile Latency (p99)";
              datasource.uid = "prometheus";
              gridPos = {
                h = 7;
                w = 8;
                x = 16;
                y = 8;
              };
              targets = [
                {
                  datasource.uid = "prometheus";
                  expr = ''histogram_quantile(0.99, sum by (le) (rate(controller_runtime_reconcile_time_seconds_bucket{controller=~"hostedgame.*"}[5m])))'';
                  legendFormat = "p99";
                  refId = "A";
                }
              ];
              fieldConfig.defaults = {
                unit = "s";
                color.mode = "palette-classic";
              };
            }
            {
              type = "row";
              title = "Backups";
              gridPos = {
                h = 1;
                w = 24;
                x = 0;
                y = 15;
              };
              collapsed = false;
            }
            {
              type = "timeseries";
              title = "Time Since Last Successful Backup";
              datasource.uid = "prometheus";
              gridPos = {
                h = 7;
                w = 12;
                x = 0;
                y = 16;
              };
              targets = [
                {
                  datasource.uid = "prometheus";
                  expr = ''time() - dionysus_hostedgame_backup_last_success_timestamp_seconds{game=~"$game"}'';
                  legendFormat = "{{game}}";
                  refId = "A";
                }
              ];
              fieldConfig.defaults = {
                unit = "s";
                color.mode = "palette-classic";
              };
            }
            {
              type = "timeseries";
              title = "Workqueue Depth";
              datasource.uid = "prometheus";
              gridPos = {
                h = 7;
                w = 12;
                x = 12;
                y = 16;
              };
              targets = [
                {
                  datasource.uid = "prometheus";
                  expr = ''sum by (name) (workqueue_depth{name=~".*osted.*|.*game.*"})'';
                  legendFormat = "{{name}}";
                  refId = "A";
                }
              ];
              fieldConfig.defaults.color.mode = "palette-classic";
            }
          ];
        };
      };
    }
  ];

  # Indices into k8sObjects — referenced by helmDeployment / helmTemplates
  # below. Documented here so reordering the list above doesn't silently
  # break the templating. Order:
  #   0: ServiceAccount
  #   1: operator Deployment
  #   2: metrics Service
  #   3: ClusterRole
  #   4: ClusterRoleBinding
  #   5: ServiceMonitor
  #   6: discord Service
  #   7: discord Deployment
  #   8: GrafanaDashboard

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
    grafana = {
      enabled = observability.grafana.enabled;
    };
    discord = discord // {
      enabled = false; # consumer opts in
      image = discord.image // {
        tag = "";
      };
    };
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

  # Discord Deployment with templated image — same pattern as
  # helmDeployment. The discord Service is rendered verbatim (its spec
  # has no values to substitute).
  discordDeployment = builtins.elemAt k8sObjects 7;
  discordContainer = builtins.elemAt discordDeployment.spec.template.spec.containers 0;

  helmDiscordDeployment = lib.recursiveUpdate discordDeployment {
    spec.template.spec.containers = [
      (
        discordContainer
        // {
          image = "{{ .Values.discord.image.repository }}:{{ .Values.discord.image.tag }}";
          imagePullPolicy = "{{ .Values.discord.image.pullPolicy }}";
        }
      )
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
    # Grafana dashboard wrapped in .Values.grafana.enabled so consumers
    # without the Grafana operator can disable it (default on).
    dashboard = pkgs.runCommand "dashboard.yaml" { } ''
      echo '{{- if .Values.grafana.enabled }}' > $out
      cat ${
        renderObjects pkgs "dashboard-objects.yaml" [
          (builtins.elemAt k8sObjects 8)
        ]
      } >> $out
      echo '{{- end }}' >> $out
    '';
    rbac = renderObjects pkgs "rbac.yaml" [
      (builtins.elemAt k8sObjects 0)
      (builtins.elemAt k8sObjects 3)
      helmOperatorClusterRoleBinding
    ];
    # discord template wraps Service + Deployment in a single Helm
    # conditional so the whole bot is opt-in via values.discord.enabled.
    # Default is false; consumers (nixlab) opt in when they have Discord
    # app credentials.
    discord = pkgs.runCommand "discord.yaml" { } ''
      echo '{{- if .Values.discord.enabled }}' > $out
      cat ${
        renderObjects pkgs "discord-objects.yaml" [
          (builtins.elemAt k8sObjects 6)
          helmDiscordDeployment
        ]
      } >> $out
      echo '{{- end }}' >> $out
    '';
  };

  # helmChart assembles the chart directory consumed by Flux's
  # HelmRelease. CRDs come from the committed charts/dionysus-operator/crds/ dir
  # (generated from Go types via controller-gen); templates come from
  # the rendered Helm-syntax objects above.
  helmChart =
    pkgs:
    let
      templates = helmTemplates pkgs;
    in
    pkgs.stdenvNoCC.mkDerivation {
      pname = "dionysus-operator-helm-chart";
      version = chart.version;
      src = ../../charts/dionysus-operator;
      dontBuild = true;
      installPhase = ''
        mkdir -p $out/templates $out/crds
        cp -r crds/. $out/crds/
        cp ${templates.deployment} $out/templates/deployment.yaml
        cp ${templates.service} $out/templates/service.yaml
        cp ${templates.observability} $out/templates/observability.yaml
        cp ${templates.rbac} $out/templates/rbac.yaml
        cp ${templates.dashboard} $out/templates/dashboard.yaml
        cp ${templates.discord} $out/templates/discord.yaml
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
      pname = "dionysus-operator-k8s-manifests";
      version = chart.version;
      dontUnpack = true;
      nativeBuildInputs = [ pkgs.yq-go ];
      installPhase = ''
        cat ${renderObjects pkgs "dionysus-operator-manifests.yaml" k8sObjects} > $out
      '';
    };
}
