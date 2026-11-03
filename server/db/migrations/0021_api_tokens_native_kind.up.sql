-- LAUN-129: a native app's login token gets its own kind, `native`.
-- Since LAUN-122 a native login made a `session` row, and the bearer header
-- accepted `session`, so the value of the web cookie also worked as a bearer
-- token (no CSRF check). Now the bearer header carries `pat` and `native`
-- only, and the cookie carries `session` only (decisions-T6a #34 to #38).
--
-- Rows made by a native login since LAUN-122 are told apart by their name:
-- only the login code writes 'native session' (and 'web session'); a PAT has
-- kind 'pat'. They become `native` here, so those apps need no new login.
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_kind_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_kind_check CHECK (kind IN ('pat', 'session', 'native'));
UPDATE api_tokens SET kind = 'native' WHERE kind = 'session' AND name = 'native session';
