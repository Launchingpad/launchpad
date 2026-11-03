-- LAUN-87: reverses 0014. The ring is a diagnostic summary, not tenant data,
-- so the down drops it.
DROP TABLE git_webhook_deliveries;
