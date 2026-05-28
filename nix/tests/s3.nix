{
  pkgs,
  self,
}: let
  # Two payloads with distinct content so we can tell which backend served each.
  s3Payload = pkgs.runCommandLocal "ncro-s3-payload" {} ''
    mkdir -p "$out"
    echo "s3 upstream test payload" > "$out/data"
  '';

  authPayload = pkgs.runCommandLocal "ncro-auth-payload" {} ''
    mkdir -p "$out"
    echo "auth upstream test payload" > "$out/data"
  '';

  # Garage cluster settings.
  # Those are fixed values for a single-node ephemeral cluster.
  # XXX: rpc_secret must be 32 bytes, hex-encoded (64 chars).
  garageRpcSecret = "c8b325ea88a9ad61e58d0ed9ba91fd0db70699e3a30ca2d8a70ee0c4a09ceaf2";
  garageRegion = "garage";
  garageBucket = "nix-cache";

  # Credentials for the nginx-protected nix-serve (auth subtest).
  authUser = "ncro";
  authPass = "testpassword";

  cacheKeyName = "ncro-s3-test";

  commonBase = {
    virtualisation.memorySize = 1536;
    virtualisation.diskSize = 4096;
    networking.firewall.enable = false;
    nix.settings.experimental-features = ["nix-command"];
  };
in
  pkgs.testers.runNixOSTest {
    name = "ncro-s3";

    nodes = {
      # Runs Garage (S3-compatible object store) pre-populated with s3Payload.
      # Also runs nix-serve behind nginx with Basic Auth for the auth subtest.
      # Garage v1.x does not support anonymous S3 API access, so an nginx
      # location on port 3903 strips the bucket prefix and proxies requests to
      # Garage's web endpoint (port 3902) which allows anonymous reads via the
      # virtual-host mechanism after `garage bucket website --allow`.
      backend = {
        config,
        pkgs,
        lib,
        ...
      }: {
        imports = [commonBase];

        system.extraDependencies = [s3Payload authPayload];

        services.garage = {
          enable = true;
          package = pkgs.garage;
          settings = {
            rpc_bind_addr = "[::]:3901";
            rpc_public_addr = "127.0.0.1:3901";
            rpc_secret = garageRpcSecret;
            # v1.x single-node mode.
            replication_mode = "none";
            s3_api = {
              s3_region = garageRegion;
              api_bind_addr = "[::]:3900";
            };
            s3_web = {
              bind_addr = "[::]:3902";
              root_domain = ".web.garage";
              index = "index.html";
            };
          };
        };

        environment.systemPackages = [pkgs.garage];

        systemd.services = {
          # Bootstrap the Garage cluster, upload s3Payload as a Nix binary cache,
          # and enable anonymous web access for the bucket.
          setup-garage = {
            description = "Initialise Garage layout, bucket, and upload Nix store paths";
            wantedBy = ["multi-user.target"];
            after = ["garage.service" "nix-daemon.service"];
            requires = ["garage.service" "nix-daemon.service"];
            path = [pkgs.garage pkgs.gawk pkgs.coreutils];
            serviceConfig = {
              Type = "oneshot";
              RemainAfterExit = true;
              ExecStart = pkgs.writeShellScript "setup-garage" ''
                set -euo pipefail

                # Wait until the Garage daemon accepts admin requests.
                until ${pkgs.garage}/bin/garage status >/dev/null 2>&1; do
                  sleep 1
                done

                # Apply a single-node layout (zone dc1, 1 GiB capacity).
                # Capture full output before filtering to avoid Garage panicking
                # on SIGPIPE from short-reading consumers like cut/awk.
                node_fqn=$(${pkgs.garage}/bin/garage node id)
                node_id=$(printf '%s\n' "$node_fqn" | cut -d@ -f1)
                ${pkgs.garage}/bin/garage layout assign -z dc1 -c 1G "$node_id"
                layout_show=$(${pkgs.garage}/bin/garage layout show)
                version=$(printf '%s\n' "$layout_show" \
                  | awk '/Current cluster layout version:/{print $NF+1}')
                ${pkgs.garage}/bin/garage layout apply --version "$version"

                # Create the bucket and enable anonymous reads via the web endpoint.
                ${pkgs.garage}/bin/garage bucket create ${garageBucket}
                ${pkgs.garage}/bin/garage bucket website --allow ${garageBucket}

                # Create an access key for nix copy uploads.
                key_output=$(${pkgs.garage}/bin/garage key create nix-upload)
                key_id=$(printf '%s\n' "$key_output" | awk '/^Key ID:/{print $3}')
                key_secret=$(printf '%s\n' "$key_output" | awk '/^Secret key:/{print $3}')
                ${pkgs.garage}/bin/garage bucket allow \
                  --read --write ${garageBucket} --key nix-upload

                # Upload the store path as a Nix binary cache.
                export AWS_ACCESS_KEY_ID=$key_id
                export AWS_SECRET_ACCESS_KEY=$key_secret
                export AWS_REGION=${garageRegion}
                ${config.nix.package}/bin/nix copy \
                    --to 's3://${garageBucket}?endpoint=127.0.0.1:3900&scheme=http&region=${garageRegion}' \
                    --no-require-sigs \
                    "${s3Payload}"
              '';
            };
          };

          gen-cache-key = {
            description = "Generate Nix binary cache signing key";
            wantedBy = ["multi-user.target"];
            before = ["nix-serve.service"];
            after = ["nix-daemon.service"];
            requires = ["nix-daemon.service"];
            serviceConfig = {
              Type = "oneshot";
              RemainAfterExit = true;
              ExecStart = pkgs.writeShellScript "gen-cache-key" ''
                set -euo pipefail
                mkdir -p /etc/nix
                if [ ! -f /etc/nix/cache-key.sec ]; then
                  ${config.nix.package}/bin/nix-store \
                    --generate-binary-cache-key "${cacheKeyName}" \
                    /etc/nix/cache-key.sec \
                    /etc/nix/cache-key.pub
                fi
                chmod 644 /etc/nix/cache-key.pub /etc/nix/cache-key.sec
                ${config.nix.package}/bin/nix store sign \
                  --key-file /etc/nix/cache-key.sec \
                  "${authPayload}"
              '';
            };
          };

          nix-serve = {
            description = "nix-serve binary cache (port 5000, plain HTTP)";
            wantedBy = ["multi-user.target"];
            after = ["gen-cache-key.service" "network.target"];
            requires = ["gen-cache-key.service"];
            environment.NIX_SECRET_KEY_FILE = "/etc/nix/cache-key.sec";
            serviceConfig = {
              ExecStart = "${pkgs.nix-serve}/bin/nix-serve --port 5000";
              Restart = "on-failure";
            };
          };
        };

        services.nginx = {
          enable = true;
          virtualHosts = {
            # Port 3903: strips /nix-cache/ prefix and proxies to Garage web
            # endpoint at port 3902.  ncro translates
            # s3://nix-cache?endpoint=backend:3903 -> http://backend:3903/nix-cache,
            # so requests arrive as GET /nix-cache/<object>; this rewrite removes
            # the bucket segment before forwarding to Garage's virtual-host web API.
            garage-web-adapter = {
              listen = [
                {
                  addr = "0.0.0.0";
                  port = 3903;
                }
              ];
              locations."~ ^/${garageBucket}/" = {
                extraConfig = ''
                  rewrite ^/${garageBucket}/(.+)$ /$1 break;
                  proxy_pass http://127.0.0.1:3902;
                  proxy_set_header Host ${garageBucket}.web.garage;
                '';
              };
            };

            # Port 8081 is for Basic Auth proxy to nix-serve for the auth subtest.
            auth-cache = {
              listen = [
                {
                  addr = "0.0.0.0";
                  port = 8081;
                }
              ];
              basicAuth = {"${authUser}" = authPass;};
              locations."/" = {proxyPass = "http://127.0.0.1:5000";};
            };
          };
        };
      };

      # ncro node with two upstreams:
      #  1. s3://nix-cache?endpoint=backend:3903&scheme=http  (S3 subtest, served
      #     through the nginx web-adapter in front of Garage)
      #  2. http://backend:8081 with username/password         (auth subtest)
      proxy = {
        imports = [self.nixosModules.ncro commonBase];

        nix.settings.trusted-substituters = ["http://localhost:8080"];

        services.ncro = {
          enable = true;
          settings = {
            server.listen = ":8080";
            upstreams = [
              {
                url = "s3://${garageBucket}?endpoint=backend:3903&scheme=http";
                priority = 1;
              }
              {
                url = "http://backend:8081";
                priority = 2;
                username = authUser;
                password = authPass;
              }
            ];
            cache = {
              ttl = "5m";
              negative_ttl = "30s";
            };
          };
        };
      };
    };

    testScript = ''
      import json

      def ncro_health(node):
          out = node.succeed("curl -sf http://localhost:8080/health")
          return json.loads(out)

      def store_hash(path):
          # /nix/store/<hash>-<name> -> <hash>
          return path.split("/")[3].split("-")[0]

      def nar_url_from_narinfo(narinfo):
          for line in narinfo.splitlines():
              if line.startswith("URL: "):
                  return line.split("URL: ", 1)[1]
          raise AssertionError(f"narinfo missing URL field: {narinfo!r}")

      s3_path   = "${s3Payload}"
      auth_path = "${authPayload}"
      s3_hash   = store_hash(s3_path)
      auth_hash = store_hash(auth_path)

      with subtest("boot all nodes"):
          start_all()

          backend.wait_for_unit("garage.service")
          backend.wait_for_open_port(3900)
          backend.wait_for_unit("setup-garage.service")
          backend.wait_for_open_port(3902)
          backend.wait_for_open_port(3903)

          backend.wait_for_unit("gen-cache-key.service")
          backend.wait_for_unit("nix-serve.service")
          backend.wait_for_open_port(5000)
          backend.wait_for_unit("nginx.service")
          backend.wait_for_open_port(8081)

          proxy.wait_for_unit("ncro.service")
          proxy.wait_for_open_port(8080)

      with subtest("Garage bucket contains nix-cache-info"):
          out = backend.succeed(
              "curl -sf -H 'Host: ${garageBucket}.web.garage' "
              "http://127.0.0.1:3902/nix-cache-info"
          )
          assert "StoreDir" in out, \
              f"nix-cache-info missing StoreDir: {out!r}"

      with subtest("Garage bucket contains narinfo for s3Payload"):
          out = backend.succeed(
              f"curl -sf -H 'Host: ${garageBucket}.web.garage' "
              f"http://127.0.0.1:3902/{s3_hash}.narinfo"
          )
          assert "StorePath" in out, \
              f"Garage narinfo missing StorePath: {out!r}"

      with subtest("auth backend rejects unauthenticated requests"):
          backend.fail(
              f"curl -sf http://127.0.0.1:8081/{auth_hash}.narinfo"
          )

      with subtest("auth backend accepts requests with correct credentials"):
          out = backend.succeed(
              f"curl -sf -u ${authUser}:${authPass} http://127.0.0.1:8081/{auth_hash}.narinfo"
          )
          assert "StorePath" in out, \
              f"auth backend did not serve narinfo with credentials: {out!r}"

      with subtest("ncro health lists both upstreams"):
          h = ncro_health(proxy)
          assert "upstreams" in h, f"/health missing upstreams: {h!r}"
          urls = [u["url"] for u in h["upstreams"]]
          # The s3:// URL is translated to http://backend:3903/nix-cache at
          # startup, so the health report uses the translated HTTP URL.
          assert any("backend:3903" in u for u in urls), \
              f"S3 upstream (translated) missing from /health: {urls}"
          assert any("backend:8081" in u for u in urls), \
              f"auth upstream missing from /health: {urls}"

      with subtest("ncro proxies narinfo from S3 upstream"):
          out = proxy.succeed(
              f"curl -sf http://localhost:8080/{s3_hash}.narinfo"
          )
          assert "StorePath" in out, \
              f"ncro did not proxy S3 narinfo: {out!r}"

      with subtest("ncro preserves byte-range responses from S3 NARs"):
          narinfo = proxy.succeed(
              f"curl -sf http://localhost:8080/{s3_hash}.narinfo"
          )
          nar_url = nar_url_from_narinfo(narinfo)
          headers = proxy.succeed(
              "curl -sS -D - -o /dev/null "
              "-H 'Range: bytes=0-15' "
              f"http://localhost:8080/{nar_url}"
          )
          header_lines = [line.rstrip("\r") for line in headers.splitlines()]
          lowered = [line.lower() for line in header_lines]
          assert any(line.startswith("HTTP/") and " 206 " in line for line in header_lines), \
              f"range request did not return 206 Partial Content: {headers!r}"
          assert any(line.startswith("content-range: bytes 0-15/") for line in lowered), \
              f"Content-Range header missing or wrong: {headers!r}"
          assert "accept-ranges: bytes" in lowered, \
              f"Accept-Ranges header missing: {headers!r}"

      with subtest("nix copy through ncro from S3 upstream"):
          proxy.fail(f"nix store ls {s3_path} 2>/dev/null")
          proxy.succeed(
              f"nix copy --from http://localhost:8080 --no-require-sigs {s3_path}"
          )
          proxy.succeed(f"test -f {s3_path}/data")
          proxy.succeed(f"grep -q 's3 upstream' {s3_path}/data")

      with subtest("ncro proxies narinfo from authenticated upstream"):
          out = proxy.succeed(
              f"curl -sf http://localhost:8080/{auth_hash}.narinfo"
          )
          assert "StorePath" in out, \
              f"ncro did not proxy auth narinfo: {out!r}"

      with subtest("nix copy through ncro from authenticated upstream"):
          proxy.fail(f"nix store ls {auth_path} 2>/dev/null")
          proxy.succeed(
              f"nix copy --from http://localhost:8080 --no-require-sigs {auth_path}"
          )
          proxy.succeed(f"test -f {auth_path}/data")
          proxy.succeed(f"grep -q 'auth upstream' {auth_path}/data")

      with subtest("ncro records cache hits after repeated requests"):
          proxy.succeed(f"curl -sf http://localhost:8080/{s3_hash}.narinfo > /dev/null")
          proxy.succeed(f"curl -sf http://localhost:8080/{auth_hash}.narinfo > /dev/null")
          metrics = proxy.succeed("curl -sf http://localhost:8080/metrics")
          assert "ncro_narinfo_cache" in metrics, \
              f"narinfo cache metric not in /metrics: {metrics[:400]!r}"
    '';
  }
