CREATE UNIQUE INDEX CONCURRENTLY idx_fdl_delivery_profile_workspace_name_active
    ON fdl_delivery_profile(workspace_id, lower(name))
    WHERE archived_at IS NULL;
