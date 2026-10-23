# Image build definition for the template.
#
# Callers MUST disable BuildKit attestations. Bake HCL cannot reliably turn
# off default provenance; scripts/build-host.{sh,ps1} always pass:
#   BUILDX_NO_DEFAULT_ATTESTATIONS=1
#   --provenance=false --sbom=false
#
# Why: Docker Desktop (BuildKit >= 0.32) attaches provenance as an OCI artifact
# whose config media type is application/vnd.oci.empty.v1+json. Aliyun ACR
# Personal / Standard rejects that class, so `docker push` fails after layers
# upload. ACR Enterprise can store OCI artifacts; if the destination becomes
# one of those registries, the script flags can be removed and provenance
# re-enabled here.

variable "SZRW_TEMPLATE_IMAGE" {
  default     = "registry.cn-shanghai.aliyuncs.com/lhdshr/szrw-template:latest"
  description = "Runtime image pushed to Aliyun ACR. Override with env SZRW_TEMPLATE_IMAGE, not IMAGE."
}

group "default" {
  targets = ["runtime"]
}

target "runtime" {
  description = "linux/amd64 production image. Attestations must stay off for ACR Personal."
  context     = "."
  dockerfile  = "Dockerfile"
  platforms   = ["linux/amd64"]
  tags        = ["${SZRW_TEMPLATE_IMAGE}"]
}
