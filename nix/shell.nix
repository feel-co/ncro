{
  mkShell,
  cargo,
  clippy,
  pkg-config,
  rust-analyzer,
  rustc,
  rustfmt,
  taplo,
  cargo-nextest,
}:
mkShell {
  name = "rust";

  strictDeps = true;
  nativeBuildInputs = [
    cargo
    rustc
    pkg-config

    rust-analyzer
    clippy
    (rustfmt.override {asNightly = true;})
    taplo

    cargo-nextest
  ];
}
