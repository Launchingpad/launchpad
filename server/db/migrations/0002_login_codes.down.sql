-- Reverses 0002_login_codes. Outstanding login codes are lost, which only
-- means a person must ask for a new one. Sessions (api_tokens) are kept.
DROP TABLE IF EXISTS login_codes;
