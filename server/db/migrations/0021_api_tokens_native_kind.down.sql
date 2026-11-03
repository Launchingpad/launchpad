-- LAUN-129: reverses 0021. A `native` row cannot stay (the old CHECK refuses
-- it), so it goes back to `session`, the kind it had before. That is the
-- nature of a down: under the old code such a token works as a bearer again.
UPDATE api_tokens SET kind = 'session' WHERE kind = 'native';
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_kind_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_kind_check CHECK (kind IN ('pat', 'session'));
