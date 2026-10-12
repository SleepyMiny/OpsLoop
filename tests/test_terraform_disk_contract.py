"""Exercise the real disk variable with Terraform, without a backend/provider.

These tests check input handling, not Azure provisioning or image capacity.
Terraform console may exit zero even when variable validation emits an error.
"""

import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


@unittest.skipUnless(shutil.which("terraform"), "Terraform CLI required")
class TerraformDiskContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        source = (ROOT / "infra/terraform/p1/variables.tf").read_text()
        # Top-level variables end at a column-zero brace; nested braces do not.
        match = re.search(r'^variable "os_disk_size_gb" \{.*?^\}', source, re.M | re.S)
        if not match:
            raise AssertionError("Disk variable block not found")
        cls.variable = match.group(0)

    def console(self, value=None, expression="var.os_disk_size_gb == null"):
        with tempfile.TemporaryDirectory(prefix="opsloop-disk-contract-") as folder:
            directory = Path(folder)
            (directory / "main.tf").write_text(self.variable + "\n")
            args = ["terraform", "console", "-no-color"]
            if value is not None:
                # Scalar -var parsing treats literal null as a string. Use HCL
                # like the actual private tfvars so null reaches the variable.
                (directory / "case.tfvars").write_text("os_disk_size_gb = " + value + "\n")
                args.append("-var-file=case.tfvars")
            # Avoid inherited CLI options, credentials or cloud configuration.
            environment = {key: os.environ[key] for key in ("PATH", "HOME", "TMPDIR") if key in os.environ}
            environment["CHECKPOINT_DISABLE"] = "1"
            return subprocess.run(args, input=expression + "\n", cwd=directory,
                                  env=environment, text=True, capture_output=True, timeout=30)

    def accepted(self, value=None, expression="var.os_disk_size_gb == null"):
        result = self.console(value, expression)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("Error:", result.stderr)
        self.assertEqual(result.stdout.strip(), "true", result.stdout)

    def rejected(self, value):
        result = self.console(value)
        self.assertIn("Invalid value for variable", result.stderr)
        self.assertIn("Disk size must be null", result.stderr)

    def test_omitted_input_inherits(self):
        self.accepted()

    def test_explicit_null_inherits_without_evaluating_floor_null(self):
        self.accepted("null")

    def test_explicit_whole_sizes_remain_supported(self):
        for size in (30, 32, 64, 4095):
            with self.subTest(size=size):
                self.accepted(str(size), f"var.os_disk_size_gb == {size}")

    def test_undersized_overrides_rejected(self):
        for size in (-1, 0, 29):
            with self.subTest(size=size):
                self.rejected(str(size))

    def test_fractional_override_rejected(self):
        self.rejected("32.5")

    def test_provider_upper_limit_enforced(self):
        self.rejected("4096")

    def test_nonnumeric_input_rejected(self):
        result = self.console('"inherit"')
        self.assertIn("Error:", result.stderr)
        self.assertIn("number is required", result.stderr)


if __name__ == "__main__":
    unittest.main()
