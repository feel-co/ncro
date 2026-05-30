{
  pkgs,
  self,
}:
pkgs.testers.runNixOSTest {
  name = "ncro-socket-activation";

  nodes.machine = {
    imports = [self.nixosModules.ncro];

    virtualisation.memorySize = 512;
    networking.firewall.enable = false;

    services.ncro = {
      enable = true;
      socketActivation = true;
      settings = {
        upstreams = [
          {
            url = "https://cache.nixos.org";
            priority = 10;
          }
        ];
      };
    };
  };

  # XXX: we don't fetch any binaries, so the pubkey for the cache is not set
  # in the ncro config. If we decided to change this test, adding the pubkey
  # might become necessary.
  testScript = ''
    with subtest("socket unit becomes active at boot"):
        machine.start()
        machine.wait_for_unit("ncro.socket")

    with subtest("ncro activates and responds on first connection"):
        # systemd holds the socket; the first HTTP request triggers activation.
        out = machine.succeed("curl -sf http://localhost:8080/nix-cache-info")
        assert "StoreDir" in out, \
            f"unexpected /nix-cache-info response: {out!r}"
        machine.wait_for_unit("ncro.service")

    with subtest("service type is notify"):
        out = machine.succeed(
            "systemctl show ncro.service --property=Type"
        )
        assert "notify" in out, \
            f"expected Type=notify, got: {out!r}"

    with subtest("health endpoint is reachable after activation"):
        import json
        out = machine.succeed("curl -sf http://localhost:8080/health")
        data = json.loads(out)
        assert "status" in data, \
            f"/health missing status field: {data!r}"
  '';
}
