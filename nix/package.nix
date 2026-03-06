{
  lib,
  buildGoModule,
}:
buildGoModule {
  pname = "ncro";
  version = "0.1.0";

  src = let
    fs = lib.fileset;
    s = ../.;
  in
    fs.toSource {
      root = s;
      fileset = fs.unions [
        (s + /cmd)
        (s + /internal)
        (s + /go.mod)
        (s + /go.sum)
      ];
    };

  vendorHash = "sha256-y4NwCPZTVaWFUzBW4Roo47pi+E0KnU/5kqnMB1rmyy8=";

  ldflags = ["-s" "-w"];
}
