-- LAUN-232 (decisions-T41-project-emoji.md): a project can carry one emoji as
-- its icon. NULL means none. The server checks that the value is one emoji
-- sequence; the CHECK is the last line of defence (not empty, at most 32 bytes).
ALTER TABLE projects
    ADD COLUMN emoji text
    CONSTRAINT projects_emoji_check CHECK (emoji IS NULL OR (emoji <> '' AND octet_length(emoji) <= 32));
