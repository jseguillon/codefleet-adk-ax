# Contract
Use integer cents. Inputs total_cents and percent are integers (booleans excluded).
Reject negative totals and percentages outside [0,100].
Final cents = floor(total_cents * (100 - percent) / 100).
Keep the contract independent of the programming language.
