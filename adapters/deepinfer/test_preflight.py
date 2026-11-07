import unittest

from preflight import validate_host


class DeepInferPreflightTests(unittest.TestCase):
    def test_requires_linux_python312_and_sm100(self):
        sm100 = ["10.0"] * 8
        validate_host("linux", (3, 12), sm100, 8)
        for os_name, python, devices in (
            ("win32", (3, 12), sm100),
            ("linux", (3, 13), sm100),
            ("linux", (3, 12), ["12.0"] * 8),
            ("linux", (3, 12), sm100[:7]),
        ):
            with self.subTest(os_name=os_name, python=python, devices=devices):
                with self.assertRaises(ValueError):
                    validate_host(os_name, python, devices, 8)
