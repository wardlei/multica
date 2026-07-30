export type FDLDeliveryRole =
  | "intake"
  | "planner"
  | "implementer"
  | "reviewer:correctness"
  | "reviewer:regression"
  | "reviewer:specialist"
  | "explorer:impact_analysis";

export interface FDLRoleBinding {
  role: FDLDeliveryRole;
  agent_id: string;
}

export interface FDLDeliveryProfile {
  id: string;
  workspace_id: string;
  name: string;
  description: string;
  squad_id: string;
  controller_config: Record<string, unknown>;
  role_bindings: FDLRoleBinding[];
  created_by: string;
  created_at: string;
  updated_at: string;
}

export interface CreateFDLDeliveryProfileRequest {
  name: string;
  description?: string;
  squad_id: string;
  controller_config: Record<string, unknown>;
  role_bindings: FDLRoleBinding[];
}

export interface UpdateFDLDeliveryProfileRequest {
  name?: string;
  description?: string;
  squad_id?: string;
  controller_config?: Record<string, unknown>;
  role_bindings?: FDLRoleBinding[];
}

export interface FDLIssueRun {
  id: string;
  issue_id: string;
  profile_id: string;
  fdl_run_id: string | null;
  status: string;
  phase: string;
  state_projection: Record<string, unknown>;
  created_at: string;
  updated_at: string;
  started_at: string | null;
  finished_at: string | null;
}
