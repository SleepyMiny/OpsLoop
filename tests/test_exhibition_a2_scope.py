import unittest
from tools.exhibition_resume import rounds_for,transition_budget
from tools.exhibition_control import ControlError
class Scope(unittest.TestCase):
 def test_a2_only(self):self.assertEqual(rounds_for({'allowedRounds':['A2']},{'allowedRounds':['A2']}),[(2,'A2')])
 def test_other_rounds_rejected(self):
  for v in (['A1'],['A2','B1'],['B1']):
   with self.assertRaises(ControlError):rounds_for({'allowedRounds':v},{'allowedRounds':v})
 def test_transition_and_cleanup_included(self):
  self.assertFalse(transition_budget('A2',0,2070));self.assertTrue(transition_budget('A2',0,2071))
