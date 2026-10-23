# Image build definition for the template.
#
# Callers MUST disable BuildKit attestations. Bake HCL cannot reliably turn
# off default provenance; scripts/build-host.{sh,ps1} always pass:
#   BUILDX_NO_DEFAULT_ATTESTATIONS=1
#   --provenance=false --sbom=false
#
# Why: the destination is the private registry registry.lmcl.xyz (needs
# `docker login` once on every machine that pushes or pulls; CI can use
# REGISTRY_USER / REGISTRY_PASSWORD). Attestations stay off: not needed for a
# personal image, saves a manifest layer, and avoids depending on how completely
# the target registry handles OCI artifacts.

variable "SZRW_TEMPLATE_IMAGE" {
  default     = "registry.lmcl.xyz/lhdshr/szrw-template:latest"
  description = "Runtime image pushed to registry.lmcl.xyz. Override with env SZRW_TEMPLATE_IMAGE, not IMAGE."
}

group "default" {
  targets = ["runtime"]
}

target "runtime" {
  description = "linux/amd64 production image. Attestations stay off (see header)."
  context     = "."
  dockerfile  = "Dockerfile"
  platforms   = ["linux/amd64"]
  tags        = ["${SZRW_TEMPLATE_IMAGE}"]
}
