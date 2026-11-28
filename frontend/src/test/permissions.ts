import type { Permission } from "../api/types";

// Resource, environment and project permissions as the API reports them: the legacy aliases plus the exact catalogue.
export const viewerAccess: Permission[] = ["read", "view"];
export const operatorAccess: Permission[] = ["read", "operate", "view", "manage_backups"];
export const editorAccess: Permission[] = ["read", "operate", "edit", "view", "read_secrets", "edit_configuration", "manage_backups"];
// Precise custom roles whose catalogue rights do not overlap.
export const editWithoutSecrets: Permission[] = ["read", "edit", "view", "edit_configuration"];
export const secretsWithoutEdit: Permission[] = ["read", "view", "read_secrets"];
