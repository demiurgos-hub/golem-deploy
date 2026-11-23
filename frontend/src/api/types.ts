// Mirrors internal/api/types.go

export interface LoginRequest {
  username: string;
  password: string;
}

export interface LoginResponse {
  status: string;
  token?: string;
  message?: string;
  detail?: string;
}

export interface RefreshResponse {
  status: string;
  token?: string;
  detail?: string;
}

export interface APIToken {
  id: number;
  name: string;
  prefix: string;
  created_at: string;
}

export interface CreateAPITokenRequest {
  name: string;
}

export interface CreateAPITokenResponse {
  status: string;
  token?: string;
  id?: number;
  name?: string;
  prefix?: string;
  created_at?: string;
  detail?: string;
}

export interface ListAPITokensResponse {
  status: string;
  tokens: APIToken[];
  detail?: string;
}

export interface RevokeAPITokenRequest {
  id: number;
}

export interface StaticFile {
  name: string;
  size: number;
  extension: string;
}

export interface VolumeInfo {
  name: string;
  driver: string;
  container_name: string;
  created_at: string;
  mountpoint: string;
  files: string[];
}

export interface DockerImage {
  id: string;
  repository: string;
  tag: string;
  size: string;
  created: string;
}

export interface RunContainerRequest {
  image_name?: string;
  container_name?: string;
  port?: string;
  env_vars?: Record<string, string>;
  label?: string;
}

export interface MassRunConfig {
  id: number;
  name: string;
  container_name: string;
  port: string;
  label: string;
  env_vars: Record<string, string>;
  created_at: string;
}

export interface AddMassRunRequest {
  name: string;
  container_name: string;
  port: string;
  label: string;
  env_vars: Record<string, string>;
}

export interface EditMassRunRequest {
  name: string;
  container_name: string;
  port: string;
  label: string;
  env_vars: Record<string, string>;
}

export interface DeleteMassRunRequest {
  name?: string;
  names?: string[];
}

export interface ReplaceMassRunRequest {
  configs: AddMassRunRequest[];
}

export interface ReplaceMassRunResponse {
  status: string;
  message?: string;
  replaced_count: number;
}

export interface ExecuteMassRunRequest {
  image_name: string;
}

export interface MassRunExecuteFailure {
  name: string;
  detail: string;
}

export interface ExecuteMassRunResponse {
  status: string;
  started: number;
  failed: MassRunExecuteFailure[];
}

// Mirrors internal/docker/container.go

export interface DockerContainer {
  container_id: string;
  image: string;
  command: string;
  created: string;
  status: string;
  ports: string;
  names: string;
  label: string;
  stats?: ContainerStats;
}

export interface ContainerStats {
  cpu_perc: string;
  mem_usage: string;
  mem_perc: string;
  net_io: string;
  block_io: string;
  pids: string;
}

export interface ContainerProcess {
  pid: string;
  user: string;
  cpu: string;
  memory: string;
  vsz: string;
  rss: string;
  tty: string;
  stat: string;
  start: string;
  time: string;
  command: string;
}

// Mirrors internal/api/types.go and internal/handlers/repo/list.go

export interface RepoJob {
  id: number;
  repo_id: number;
  kind: string;
  status: string;
  detail?: string;
  created_at: string;
  finished_at?: string;
}

export interface RepoJobResponse {
  status: string;
  job?: RepoJob;
  detail?: string;
}

export interface Repository {
  id: number;
  name: string;
  url: string;
  branch: string;
  auth_type: string;
  last_commit?: string;
  last_pulled?: string;
  status: string;
  created_at: string;
  repo_size: string;
  repo_bytes_size: number;
}

export interface UnapprovedAccount {
  id: number;
  username: string;
  access_level: number;
  created_at: string;
}
