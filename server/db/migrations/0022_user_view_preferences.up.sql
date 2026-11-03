-- LAUN-141: the remembered issue-page view (layout and sort) per person, org
-- and place, so it follows the person across devices (decisions-T23-view-preferences).
-- `changed_at` is the CLIENT's time of the choice in epoch milliseconds (clamped
-- to the server clock on write): a write that is not newer than the stored one is ignored.
CREATE TABLE user_view_preferences (
    org_id     uuid   NOT NULL,
    user_id    uuid   NOT NULL,
    place      text   NOT NULL CONSTRAINT user_view_preferences_place_check CHECK (place ~ '^[a-z0-9._:-]{1,200}$'),
    view       text   NOT NULL CONSTRAINT user_view_preferences_view_check CHECK (view IN ('list', 'board')),
    sort       text   NOT NULL CONSTRAINT user_view_preferences_sort_check CHECK (sort IN ('updated', 'created', 'priority', 'manual')),
    changed_at bigint NOT NULL CONSTRAINT user_view_preferences_changed_check CHECK (changed_at >= 0),
    PRIMARY KEY (org_id, user_id, place),
    CONSTRAINT user_view_preferences_member_fk FOREIGN KEY (org_id, user_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE
);
