{
  config,
  pkgs,
  lib,
  necroPackage,
  ...
}: let
  inherit (lib.modules) mkIf;
  inherit (lib.options) mkOption mkEnableOption literalExpression;
  inherit (lib.types) bool package;

  tomlFormat = pkgs.formats.toml {};
  tomlType = tomlFormat.type;

  cfg = config.services.ncro;
  configFile = tomlFormat.generate "ncro.toml" cfg.settings;
  upstreamPublicKeys = lib.pipe (cfg.settings.upstreams or []) [
    (builtins.map (upstream: upstream.public_key or ""))
    (builtins.filter (key: key != ""))
    lib.unique
  ];
in {
  options.services.ncro = {
    enable = mkEnableOption "ncro, the Nix cache route optimizer";

    addUpstreamPublicKeys = mkOption {
      type = bool;
      default = true;
      description = ''
        Append non-empty upstream public_key values from {option}`services.ncro.settings`
        to {option}`nix.settings.trusted-public-keys`.

        This keeps Nix client signature validation aligned with the upstream
        caches that ncro is allowed to route to. Disable this if you manage Nix
        trusted public keys separately.
      '';
    };

    package = mkOption {
      type = package;
      default = necroPackage;
      defaultText = literalExpression "inputs.ncro.packages.$${system}.ncro";
      description = "The ncro package to use.";
      example = literalExpression "inputs.ncro.packages.$${system}.ncro";
    };

    settings = mkOption {
      type = tomlType;
      default = {};
      description = ''
        ncro configuration as an attribute set.

        Keys and structure match the TOML config file format; all defaults are
        handled by the ncro binary.
      '';
      example = {
        logging.level = "info";
        server = {
          listen = ":8080";
          cache_priority = 20;
        };

        upstreams = [
          {
            url = "https://cache.nixos.org";
            priority = 10;
          }
          {
            url = "https://nix-community.cachix.org";
            priority = 20;
          }
        ];

        cache = {
          ttl = "2h";
          negative_ttl = "15m";
        };
      };
    };
  };

  config = mkIf cfg.enable {
    nix.settings.trusted-public-keys = mkIf cfg.addUpstreamPublicKeys (lib.mkAfter upstreamPublicKeys);

    systemd.services.ncro = {
      description = "Nix Cache Route Optimizer";
      wantedBy = ["multi-user.target"];
      after = ["network.target"];
      serviceConfig = {
        ExecStart = "${lib.getExe' cfg.package "ncro"} --config ${configFile}";
        DynamicUser = true;
        StateDirectory = "ncro";
        Restart = "on-failure";
        RestartSec = "5s";

        # Hardening
        NoNewPrivileges = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectProc = "invisible";
        ProtectHostname = true;
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectKernelLogs = true;
        ProtectKernelTunables = true;
        RestrictRealtime = true;
        CapabilityBoundingSet = "";
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_NETLINK" # required by mdns-sd and system resolver
        ];
        RestrictNamespaces = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallFilter = ["@system-service"];
        SystemCallArchitectures = "native";
      };
    };
  };
}
