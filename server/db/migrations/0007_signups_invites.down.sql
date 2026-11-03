-- Reverses 0007_signups_invites. Pending sign-up requests and open invites
-- are lost: a server without this migration has neither flow, and the
-- people must ask (or be invited) again after a new up. Users and
-- memberships that an approval or an accepted invite made are NOT removed:
-- they are ordinary users rows. users.email_unproven is dropped: an account
-- made from a copied invite link, or approved without a confirmed email,
-- then keeps its sessions at its first email-code login.
DROP TABLE IF EXISTS org_invites;
DROP TABLE IF EXISTS signup_requests;
ALTER TABLE users DROP COLUMN IF EXISTS email_unproven;
