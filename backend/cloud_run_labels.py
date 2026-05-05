from checkov.terraform.checks.resource.base_resource_check import BaseResourceCheck
from checkov.common.models.enums import CheckResult, CheckCategories

class CloudRunHasLabels(BaseResourceCheck):
    def __init__(self):
        # This custom check ensures Cloud Run services have labels defined.
        # Labels are critical for cost allocation and governance in GCP.
        name = "Ensure Cloud Run services have labels"
        id = "CKV_CUSTOM_001"
        supported_resources = ("google_cloud_run_v2_service", "google_cloud_run_service")
        categories = (CheckCategories.GENERAL,)
        super().__init__(name=name, id=id, categories=categories, supported_resources=supported_resources)

    def scan_resource_conf(self, conf):
        # Check for labels at the top level or within the template block
        if "labels" in conf:
            return CheckResult.PASSED
        if "template" in conf and isinstance(conf["template"], list) and "labels" in conf["template"][0]:
            return CheckResult.PASSED
        return CheckResult.FAILED

check = CloudRunHasLabels()