{
  lib,
  stdenv,
  mkShell,
  cargo,
  clang,
  clippy,
  pkg-config,
  rust-analyzer,
  rustc,
  rustfmt,
  taplo,
  cargo-nextest,
  wild,
  openssl,
}: let
  # wild + clang are only used on Linux tier-1 arches
  hasWild = plat: plat.isLinux && (plat.isx86_64 || plat.isAarch64);
in
  mkShell {
    name = "rust";

    strictDeps = true;
    nativeBuildInputs =
      [
        cargo
        rustc
        pkg-config

        rust-analyzer
        clippy
        (rustfmt.override {asNightly = true;})
        taplo

        cargo-nextest
      ]
      ++ lib.optionals (hasWild stdenv.hostPlatform) [
        wild
        clang
      ];

    buildInputs = [openssl.dev];

    env =
      {
        OPENSSL_NO_VENDOR = 1;
      }
      // lib.optionalAttrs (hasWild stdenv.hostPlatform) {
        RUSTFLAGS = "-Clinker=${clang}/bin/clang -Clink-arg=--ld-path=wild";
      };
  }
