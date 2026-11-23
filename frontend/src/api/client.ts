import type {
  LoginRequest,
  LoginResponse,
  RefreshResponse,
  StaticFile,
  VolumeInfo,
  DockerImage,
  DockerContainer,
  ContainerProcess,
  RunContainerRequest,
  MassRunConfig,
  AddMassRunRequest,
  EditMassRunRequest,
  ReplaceMassRunResponse,
  ExecuteMassRunResponse,
  Repository,
  RepoJob,
  UnapprovedAccount,
  APIToken,
  CreateAPITokenResponse,
} from "./types";

type OperationCallback = (ops: Map<string, string>) => void;

class ApiClient {
  private token: string | null = null;
  private opCounter = 0;
  private activeOps = new Map<string, string>();
  private listeners = new Set<OperationCallback>();

  setToken(token: string | null) {
    this.token = token;
  }

  getToken() {
    return this.token;
  }

  onOperationsChange(cb: OperationCallback) {
    this.listeners.add(cb);
    return () => this.listeners.delete(cb);
  }

  private notifyListeners() {
    const snapshot = new Map(this.activeOps);
    this.listeners.forEach((cb) => cb(snapshot));
  }

  private startOp(label: string): string {
    const id = `op-${++this.opCounter}`;
    this.activeOps.set(id, label);
    this.notifyListeners();
    return id;
  }

  private endOp(id: string) {
    this.activeOps.delete(id);
    this.notifyListeners();
  }

  private async request<T>(
    method: string,
    endpoint: string,
    body?: unknown,
    track = true,
  ): Promise<T> {
    const shouldTrack = track && !endpoint.includes("/list");
    let opId: string | undefined;
    if (shouldTrack) {
      opId = this.startOp(`${method} ${endpoint}`);
    }

    try {
      const headers: Record<string, string> = {
        "Content-Type": "application/json",
      };
      if (this.token) {
        headers["Authorization"] = `Bearer ${this.token}`;
      }

      const resp = await fetch(endpoint, {
        method,
        headers,
        body: body ? JSON.stringify(body) : undefined,
      });

      const text = await resp.text();
      if (resp.status >= 400) {
        throw new Error(text || `API error (${resp.status})`);
      }

      return text ? (JSON.parse(text) as T) : ({} as T);
    } finally {
      if (opId) this.endOp(opId);
    }
  }

  private async uploadMultipart<T>(
    endpoint: string,
    formData: FormData,
  ): Promise<T> {
    const opId = this.startOp(`POST ${endpoint}`);
    try {
      const headers: Record<string, string> = {};
      if (this.token) {
        headers["Authorization"] = `Bearer ${this.token}`;
      }

      const resp = await fetch(endpoint, {
        method: "POST",
        headers,
        body: formData,
      });

      const text = await resp.text();
      if (resp.status >= 400) {
        throw new Error(text || `API error (${resp.status})`);
      }
      return text ? (JSON.parse(text) as T) : ({} as T);
    } finally {
      this.endOp(opId);
    }
  }

  private async downloadBlob(
    method: string,
    endpoint: string,
    body?: unknown,
  ): Promise<Blob> {
    const opId = this.startOp(`${method} ${endpoint}`);
    try {
      const headers: Record<string, string> = {};
      if (body) headers["Content-Type"] = "application/json";
      if (this.token) headers["Authorization"] = `Bearer ${this.token}`;

      const resp = await fetch(endpoint, {
        method,
        headers,
        body: body ? JSON.stringify(body) : undefined,
      });

      if (resp.status >= 400) {
        const text = await resp.text();
        throw new Error(text || `API error (${resp.status})`);
      }

      return await resp.blob();
    } finally {
      this.endOp(opId);
    }
  }

  triggerDownload(blob: Blob, filename: string) {
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
  }

  /** Local date/time as YYYY-MM-DD_HH-MM-SS, used in downloaded log filenames. */
  private logDownloadTimestamp(): string {
    const d = new Date();
    const p = (n: number) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}_${p(d.getHours())}-${p(d.getMinutes())}-${p(d.getSeconds())}`;
  }

  // Auth
  async login(username: string, password: string): Promise<LoginResponse> {
    const req: LoginRequest = { username, password };
    const resp = await this.request<LoginResponse>("POST", "/login", req);
    if (resp.status === "success" && resp.token) {
      this.setToken(resp.token);
    }
    return resp;
  }

  async createAccount(username: string, password: string): Promise<void> {
    await this.request("POST", "/account/create", {
      username,
      password,
      access_level: 1,
      approved: false,
    });
  }

  async refresh(): Promise<RefreshResponse> {
    const resp = await this.request<RefreshResponse>(
      "POST",
      "/account/refresh",
    );
    if (resp.status === "success" && resp.token) {
      this.setToken(resp.token);
    }
    return resp;
  }

  async changePassword(
    oldPassword: string,
    newPassword: string,
  ): Promise<void> {
    await this.request("POST", "/account/password", {
      old_password: oldPassword,
      new_password: newPassword,
    });
  }

  async getUnapprovedAccounts(): Promise<UnapprovedAccount[]> {
    const resp = await this.request<{
      status: string;
      accounts: UnapprovedAccount[];
    }>("GET", "/account/unapproved");
    return resp.accounts ?? [];
  }

  async approveAccount(username: string, approve: boolean): Promise<void> {
    await this.request("POST", "/account/approve", { username, approve });
  }

  async getAllAccounts(): Promise<UnapprovedAccount[]> {
    const resp = await this.request<{
      status: string;
      accounts: UnapprovedAccount[];
    }>("GET", "/account/list");
    return resp.accounts ?? [];
  }

  async deleteAccount(username: string): Promise<void> {
    await this.request("POST", "/account/delete", { username });
  }

  async createAPIToken(name: string): Promise<CreateAPITokenResponse> {
    return this.request<CreateAPITokenResponse>("POST", "/account/tokens", {
      name,
    });
  }

  async listAPITokens(): Promise<APIToken[]> {
    const resp = await this.request<{
      status: string;
      tokens: APIToken[];
    }>("GET", "/account/tokens", undefined, false);
    return resp.tokens ?? [];
  }

  async revokeAPIToken(id: number): Promise<void> {
    await this.request("POST", "/account/tokens/revoke", { id });
  }

  // Containers
  async listContainers(): Promise<DockerContainer[]> {
    const resp = await this.request<{ containers: DockerContainer[] }>(
      "GET",
      "/docker/container/list",
    );
    return resp.containers ?? [];
  }

  async startContainer(containerName: string): Promise<void> {
    await this.request("POST", "/docker/container/start", {
      container_name: containerName,
    });
  }

  async stopContainer(containerName: string): Promise<void> {
    await this.request("POST", "/docker/container/stop", {
      container_name: containerName,
    });
  }

  async deleteContainers(containerNames: string[]): Promise<void> {
    await this.request("POST", "/docker/container/delete", {
      container_names: containerNames,
    });
  }

  async getContainerProcesses(
    containerName: string,
  ): Promise<ContainerProcess[]> {
    const resp = await this.request<{
      status: string;
      processes: ContainerProcess[];
    }>("POST", "/docker/container/processes", {
      container_name: containerName,
    });
    return resp.processes ?? [];
  }

  async downloadContainerLogs(
    containerName: string,
    label?: string,
  ): Promise<void> {
    const blob = await this.downloadBlob(
      "POST",
      "/docker/container/logs",
      { container_name: containerName },
    );
    const ts = this.logDownloadTimestamp();
    const parts: string[] = [];
    const trimmedLabel = label?.trim();
    if (trimmedLabel) parts.push(trimmedLabel);
    if (containerName) parts.push(containerName);
    parts.push(ts);
    this.triggerDownload(blob, `${parts.join("-")}.log`);
  }

  async runContainer(req: RunContainerRequest): Promise<void> {
    await this.request("POST", "/docker/container/run", req);
  }

  // Images
  async listImages(): Promise<DockerImage[]> {
    const resp = await this.request<{ images: DockerImage[] }>(
      "GET",
      "/docker/image/list",
    );
    return resp.images ?? [];
  }

  async deleteImages(imageIds: string[]): Promise<void> {
    await this.request("POST", "/docker/image/delete", {
      image_ids: imageIds,
    });
  }

  // Static files
  async listStaticFiles(): Promise<StaticFile[]> {
    const resp = await this.request<{ files: StaticFile[] }>(
      "GET",
      "/build/static/list",
    );
    return resp.files ?? [];
  }

  async deleteStaticFiles(fileNames: string[]): Promise<void> {
    await this.request("POST", "/build/static/delete", {
      file_names: fileNames,
    });
  }

  async uploadStaticFiles(files: File[]): Promise<void> {
    const formData = new FormData();
    files.forEach((f) => formData.append("files", f));
    await this.uploadMultipart("/build/static/upload", formData);
  }

  async downloadStaticFile(fileName: string): Promise<void> {
    const blob = await this.downloadBlob(
      "GET",
      `/build/static/download?file_name=${encodeURIComponent(fileName)}`,
    );
    this.triggerDownload(blob, fileName);
  }

  // Volumes
  async listVolumes(): Promise<VolumeInfo[]> {
    const resp = await this.request<{
      status: string;
      volumes: VolumeInfo[];
    }>("GET", "/docker/volume/list");
    return resp.volumes ?? [];
  }

  async listOrphanedVolumes(): Promise<VolumeInfo[]> {
    const resp = await this.request<{
      status: string;
      orphaned_volumes: VolumeInfo[];
    }>("GET", "/docker/volume/orphaned");
    return resp.orphaned_volumes ?? [];
  }

  async getVolumeInfo(containerName: string): Promise<VolumeInfo> {
    const resp = await this.request<{ status: string; volume: VolumeInfo }>(
      "GET",
      `/docker/volume/info?container_name=${encodeURIComponent(containerName)}`,
    );
    return resp.volume;
  }

  async deleteVolumes(volumeNames: string[]): Promise<void> {
    await this.request("POST", "/docker/volume/delete", {
      volume_names: volumeNames,
    });
  }

  // Repositories
  async listRepositories(): Promise<Repository[]> {
    const resp = await this.request<{
      status: string;
      repositories: Repository[];
    }>("GET", "/repo/list");
    return resp.repositories ?? [];
  }

  async addRepository(
    name: string,
    url: string,
    branch: string,
    authType: string,
    username: string,
    password: string,
    sshKey: string,
  ): Promise<void> {
    await this.request("POST", "/repo/add", {
      name,
      url,
      branch,
      auth_type: authType,
      username,
      password,
      ssh_key: sshKey,
    });
  }

  async pullRepository(repoId: number): Promise<void> {
    await this.request("POST", "/repo/pull", { repo_id: repoId });
  }

  async getRepoJob(jobId: number): Promise<RepoJob> {
    const resp = await this.request<{
      status: string;
      job: RepoJob;
    }>("GET", `/repo/job?job_id=${jobId}`, undefined, false);
    return resp.job;
  }

  async switchRepositoryBranch(
    repoId: number,
    branch: string,
  ): Promise<void> {
    await this.request("POST", "/repo/branch", {
      repo_id: repoId,
      branch,
    });
  }

  async deleteRepository(repoId: number): Promise<void> {
    await this.request(
      "DELETE",
      `/repo/delete?repo_id=${repoId}`,
    );
  }

  // Mass Run
  async listMassRunConfigs(): Promise<MassRunConfig[]> {
    const resp = await this.request<{
      status: string;
      configs: MassRunConfig[];
    }>("GET", "/massrun/list");
    return resp.configs ?? [];
  }

  async addMassRunConfig(config: AddMassRunRequest): Promise<void> {
    await this.request("POST", "/massrun/add", config);
  }

  async editMassRunConfig(config: EditMassRunRequest): Promise<void> {
    await this.request("POST", "/massrun/edit", config);
  }

  async deleteMassRunConfigs(names: string[]): Promise<void> {
    await this.request("POST", "/massrun/delete", { names });
  }

  async deleteMassRunConfig(name: string): Promise<void> {
    await this.deleteMassRunConfigs([name]);
  }

  async replaceMassRunConfigs(
    configs: AddMassRunRequest[],
  ): Promise<ReplaceMassRunResponse> {
    return await this.request<ReplaceMassRunResponse>(
      "POST",
      "/massrun/replace",
      { configs },
    );
  }

  async executeMassRun(imageName: string): Promise<ExecuteMassRunResponse> {
    return await this.request<ExecuteMassRunResponse>(
      "POST",
      "/massrun/execute",
      { image_name: imageName },
    );
  }
}

export const apiClient = new ApiClient();
