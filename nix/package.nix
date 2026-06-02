{
  lib,
  rustPlatform,
  pkg-config,
  cacert,
}: let
  cargoTOML = (lib.importTOML ../Cargo.toml).workspace.package;
in
  rustPlatform.buildRustPackage (finalAttrs: {
    pname = "ncro";
    inherit (cargoTOML) version;

    src = let
      fs = lib.fileset;
      s = ../.;
    in
      fs.toSource {
        root = s;
        fileset = fs.unions [
          (s + /ncro)
          (s + /crates)
          (s + /Cargo.toml)
          (s + /Cargo.lock)
        ];
      };

    useNextest = true;
    cargoLock.lockFile = "${finalAttrs.src}/Cargo.lock";
    nativeBuildInputs = [pkg-config cacert];

    # reqwest (rustls) needs a CA bundle to construct a TLS client, even in
    # tests that never make network requests.
    env.SSL_CERT_FILE = "${cacert}/etc/ssl/certs/ca-bundle.crt";

    meta = {
      homepage = "https://github.com/feel-co/ncro";
      license = lib.licenses.eupl12;
      mainProgram = "ncro";
      maintainers = with lib.maintainers; [NotAShelf];
    };
  })
