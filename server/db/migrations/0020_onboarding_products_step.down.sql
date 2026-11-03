-- LAUN-67: reverses 0020. A stored skip of the "products" step is deleted first
-- (the old CHECK would refuse it), which is the nature of a down.
DELETE FROM onboarding_skips WHERE step = 'products';
ALTER TABLE onboarding_skips DROP CONSTRAINT onboarding_skips_step_check;
ALTER TABLE onboarding_skips ADD CONSTRAINT onboarding_skips_step_check CHECK (step IN
    ('organisation', 'mail', 'signups', 'team', 'agent', 'import', 'githost', 'invite'));
