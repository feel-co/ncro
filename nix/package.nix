{
  lib,
  rustPlatform,
  pkg-config,
}:
rustPlatform.buildRustPackage (finalAttrs: {
  pname = "ncro";
  version = "2.0.0";

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

  # relys on ca certificates
  checkFlags = ''
      "--skip=ncro::tests::ema_and_status_progression"
    '';

  cargoLock.lockFile = "${finalAttrs.src}/Cargo.lock";
  nativeBuildInputs = [pkg-config];

  meta = {
    homepage = "https://github.com/feel-co/ncro";
    license = lib.licenses.eupl12;
    mainProgram = "ncro";
    maintainers = with lib.maintainers; [NotAShelf];
  };
})
