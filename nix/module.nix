self: {
  config,
  pkgs,
  lib,
  ...
}: let
  inherit (lib.modules) mkIf;
  inherit (lib.options) mkOption mkEnableOption mkPackageOption;

  format = pkgs.formats.yaml {};

  cfg = config.services.ncro;
  configFile = format.generate "ncro.yaml" cfg.settings;
in {
  options.services.ncro = {
    enable = mkEnableOption "ncro, the Nix cache route optimizer";

    package = mkPackageOption self.packages.${pkgs.stdenv.hostPlatform.system} ["ncro"] {};

    settings = mkOption {
      type = format.type;
      default = {};
      description = ''
        ncro configuration as an attribute set. Keys and structure match the
        YAML config file format; all defaults are handled by the ncro binary.
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
    systemd.services.ncro = {
      description = "Nix Cache Route Optimizer";
      wantedBy = ["multi-user.target"];
      after = ["network.target"];
      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} -config ${configFile}";
        DynamicUser = true;
        StateDirectory = "ncro";
        Restart = "on-failure";
        RestartSec = "5s";
      };
    };
  };
}
