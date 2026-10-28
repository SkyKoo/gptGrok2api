import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common.browser import browser_context_kwargs, browser_kwargs


class BrowserArgumentLayerTests(unittest.TestCase):
    def setUp(self):
        self.profile = {
            "user_agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/131.0.0.0 Safari/537.36",
            "navigator_language": "en-US",
            "timezone_iana": "America/Phoenix",
            "inner_width": 1440,
            "inner_height": 900,
            "device_pixel_ratio": 2,
        }

    def test_launch_kwargs_do_not_contain_context_options(self):
        launch = browser_kwargs(
            "TURNSTILE",
            "http://proxy.example:8080",
            profile=self.profile,
        )

        self.assertEqual(launch["proxy"], "http://proxy.example:8080")
        self.assertEqual(launch["timezone"], "America/Phoenix")
        self.assertEqual(launch["locale"], "en-US")
        self.assertNotIn("user_agent", launch)
        self.assertNotIn("viewport", launch)
        self.assertNotIn("device_scale_factor", launch)

    def test_context_kwargs_contain_saved_profile_environment(self):
        context = browser_context_kwargs(self.profile)

        self.assertEqual(context["user_agent"], self.profile["user_agent"])
        self.assertEqual(context["viewport"], {"width": 1440, "height": 900})
        self.assertEqual(context["device_scale_factor"], 2.0)

    def test_invalid_context_values_are_dropped(self):
        context = browser_context_kwargs(
            {
                "user_agent": "short",
                "inner_width": "not-a-number",
                "inner_height": 100,
                "device_pixel_ratio": 9,
            }
        )

        self.assertEqual(context, {})


if __name__ == "__main__":
    unittest.main()
