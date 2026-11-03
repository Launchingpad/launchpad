-- LAUN-67 (docs/internal/decisions-T18-products.md #32, #37): the optional checklist step
-- "products" ("Group your teams into products"). A skip of it needs the id in the CHECK.
ALTER TABLE onboarding_skips DROP CONSTRAINT onboarding_skips_step_check;
ALTER TABLE onboarding_skips ADD CONSTRAINT onboarding_skips_step_check CHECK (step IN
    ('organisation', 'mail', 'signups', 'team', 'agent', 'import', 'githost', 'invite', 'products'));
