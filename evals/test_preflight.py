"""Checks the decision of the preflight: when is a key confined to one model?"""

import unittest

from evals.preflight import ALLOWED_MODEL, AllowedModelMissing, Confined, NotConfined, failure_message, judge


class JudgeTests(unittest.TestCase):
    def test_a_key_that_lists_only_the_model_under_test_is_confined(self) -> None:
        self.assertEqual(judge([ALLOWED_MODEL]), Confined(ALLOWED_MODEL))

    def test_a_key_that_lists_any_other_model_is_not_confined_and_the_other_models_are_named(self) -> None:
        verdict = judge(["z/last", ALLOWED_MODEL, "a/first", "m/middle", "b/second"])
        self.assertEqual(verdict, NotConfined(extra_models=4, examples=("a/first", "b/second", "m/middle")))

    def test_a_key_that_cannot_use_the_model_under_test_is_not_accepted_even_though_it_is_confined(self) -> None:
        # A run on such a key would fail on its first request; the gate says why.
        self.assertEqual(judge([]), AllowedModelMissing(listed=0))
        self.assertEqual(judge(["other/model"]), AllowedModelMissing(listed=1))


class FailureMessageTests(unittest.TestCase):
    def test_a_confined_key_has_no_failure(self) -> None:
        self.assertIsNone(failure_message(Confined(ALLOWED_MODEL)))

    def test_an_unconfined_key_is_refused_with_the_other_models_and_the_steps_to_confine_it(self) -> None:
        message = failure_message(judge([ALLOWED_MODEL, "openai/gpt-4o", "google/gemini"]))
        self.assertIn("2 other models", message)
        self.assertIn("google/gemini, openai/gpt-4o", message)
        self.assertIn("Model allowlist: " + ALLOWED_MODEL, message)

    def test_a_key_without_the_model_under_test_is_refused_and_says_so(self) -> None:
        self.assertIn(f"{ALLOWED_MODEL} is not one of them", failure_message(judge(["other/model"])))


if __name__ == "__main__":
    unittest.main()
