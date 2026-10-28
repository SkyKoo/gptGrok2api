import sys
import types
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.modules.setdefault("cloakbrowser", types.ModuleType("cloakbrowser"))

import xai_flow


class XaiEmailErrorTests(unittest.TestCase):
    def test_classifies_email_signup_unavailable_page(self):
        self.assertEqual(
            xai_flow._classify_email_stage_error(
                "Email sign-up isn’t available right now. Sign up another way."
            ),
            "signup_unavailable",
        )

    def test_maps_upstream_email_signup_error_to_safe_code(self):
        self.assertEqual(
            xai_flow._upstream_email_error("account:email-signup-unavailable"),
            ("signup_unavailable", "email_signup_unavailable"),
        )
        self.assertIsNone(xai_flow._upstream_email_error("password=secret"))


if __name__ == "__main__":
    unittest.main()
