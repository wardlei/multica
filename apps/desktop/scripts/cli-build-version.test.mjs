import { describe, expect, it } from "vitest";

import { daemonVersionFromDescribe } from "./cli-build-version.mjs";

describe("daemonVersionFromDescribe", () => {
  it("turns an untagged source-build hash into the accepted dev shape", () => {
    expect(daemonVersionFromDescribe("058b01f4")).toBe("v0.0.0-0-g058b01f4");
  });

  it("keeps an untagged dirty source build recognizable", () => {
    expect(daemonVersionFromDescribe("058b01f4-dirty")).toBe(
      "v0.0.0-0-g058b01f4-dirty",
    );
  });

  it("preserves release and already-described versions", () => {
    expect(daemonVersionFromDescribe("v0.2.21")).toBe("v0.2.21");
    expect(daemonVersionFromDescribe("v0.2.21-3-g058b01f4")).toBe(
      "v0.2.21-3-g058b01f4",
    );
  });
});
