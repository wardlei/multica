// Normalizes the source-build version recorded in the bundled daemon binary.
// Capability gates accept release semver or git-describe's between-tag shape.
export function daemonVersionFromDescribe(describe) {
  const match = /^([0-9a-fA-F]{7,})(-dirty)?$/.exec(describe);
  if (match) return `v0.0.0-0-g${match[1]}${match[2] ?? ""}`;
  return describe;
}
