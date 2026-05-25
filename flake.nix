{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs?ref=nixos-unstable";

  outputs = {
    self,
    nixpkgs,
    ...
  }: let
    systems = ["x86_64-linux" "aarch64-linux"];
    forEachSystem = nixpkgs.lib.genAttrs systems;
    pkgsForEach = nixpkgs.legacyPackages;
  in {
    packages = forEachSystem (system: {
      ncro = pkgsForEach.${system}.callPackage ./nix/package.nix {};
      default = self.packages.${system}.ncro;
    });

    devShells = forEachSystem (system: {
      default = pkgsForEach.${system}.callPackage ./nix/shell.nix {};
    });

    nixosModules = {
      ncro = ./nix/module.nix;
      default = self.nixosModules.ncro;
    };

    hydraJobs = self.packages;

    checks = forEachSystem (system: {
      p2p-discovery = import ./nix/tests/p2p.nix {
        pkgs = pkgsForEach.${system};
        inherit self;
      };
    });
  };
}
