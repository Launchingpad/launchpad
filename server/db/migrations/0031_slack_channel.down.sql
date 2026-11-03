-- LAUN-198: reverses 0031. It loses the stored webhook (sealed) and any post
-- still waiting, which is the nature of a down.
DROP TABLE slack_outbox;
DROP TABLE slack_channels;
