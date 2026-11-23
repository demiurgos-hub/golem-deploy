import { useState, useCallback, createContext, useContext } from "react";
import * as Tabs from "@radix-ui/react-tabs";
import { RefreshCw, LogOut, Sun, Moon } from "lucide-react";
import { useAuth } from "@/hooks/use-auth";
import { useTheme } from "@/hooks/use-theme";
import { OperationsFooter } from "./operations-footer";
import { ContainersTab } from "@/pages/containers";
import { ImagesTab } from "@/pages/images";
import { StaticFilesTab } from "@/pages/static-files";
import { SavesTab } from "@/pages/saves";
import { RepositoriesTab } from "@/pages/repositories";
import { MassRunTab } from "@/pages/mass-run";
import { AccountTab } from "@/pages/account";

interface DashboardContextValue {
  activeTab: string;
  refreshKey: number;
  navigateToSaves: (containerName: string) => void;
}

const DashboardContext = createContext<DashboardContextValue>({
  activeTab: "containers",
  refreshKey: 0,
  navigateToSaves: () => {},
});

export function useDashboard() {
  return useContext(DashboardContext);
}

const TAB_ITEMS = [
  { value: "containers", label: "Containers" },
  { value: "images", label: "Images" },
  { value: "static", label: "Static" },
  { value: "saves", label: "Saves" },
  { value: "repositories", label: "Repositories" },
  { value: "massrun", label: "Mass Run" },
  { value: "account", label: "Account" },
];

export function DashboardLayout() {
  const { logout } = useAuth();
  const { dark, toggle: toggleTheme } = useTheme();
  const [activeTab, setActiveTab] = useState("containers");
  const [refreshKey, setRefreshKey] = useState(0);
  const [savesContainerName, setSavesContainerName] = useState<string | null>(
    null,
  );

  const handleRefresh = useCallback(() => {
    setRefreshKey((k) => k + 1);
  }, []);

  const navigateToSaves = useCallback((containerName: string) => {
    setSavesContainerName(containerName);
    setActiveTab("saves");
  }, []);

  return (
    <DashboardContext.Provider value={{ activeTab, refreshKey, navigateToSaves }}>
      <div className="flex h-screen flex-col">
        <Tabs.Root
          value={activeTab}
          onValueChange={setActiveTab}
          className="flex flex-1 flex-col overflow-hidden"
        >
          {/* Top bar */}
          <div className="flex items-center justify-between border-b border-border px-4 py-2">
            <Tabs.List className="flex gap-1 overflow-x-auto">
              {TAB_ITEMS.map((tab) => (
                <Tabs.Trigger
                  key={tab.value}
                  value={tab.value}
                  className="whitespace-nowrap rounded-md px-3 py-1.5 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground data-[state=active]:bg-secondary data-[state=active]:text-foreground"
                >
                  {tab.label}
                </Tabs.Trigger>
              ))}
            </Tabs.List>

            <div className="flex items-center gap-1 ml-2">
              <button
                onClick={handleRefresh}
                className="rounded-md p-2 text-muted-foreground hover:bg-accent hover:text-foreground"
                title="Refresh"
              >
                <RefreshCw className="h-4 w-4" />
              </button>
              <button
                onClick={toggleTheme}
                className="rounded-md p-2 text-muted-foreground hover:bg-accent hover:text-foreground"
                title={dark ? "Light Theme" : "Dark Theme"}
              >
                {dark ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
              </button>
              <button
                onClick={logout}
                className="rounded-md p-2 text-muted-foreground hover:bg-accent hover:text-foreground"
                title="Logout"
              >
                <LogOut className="h-4 w-4" />
              </button>
            </div>
          </div>

          {/* Tab content */}
          <div className="flex-1 overflow-auto p-4">
            <Tabs.Content value="containers" className="h-full outline-none">
              <ContainersTab />
            </Tabs.Content>
            <Tabs.Content value="images" className="h-full outline-none">
              <ImagesTab />
            </Tabs.Content>
            <Tabs.Content value="static" className="h-full outline-none">
              <StaticFilesTab />
            </Tabs.Content>
            <Tabs.Content value="saves" className="h-full outline-none">
              <SavesTab pendingContainerName={savesContainerName} onConsumed={() => setSavesContainerName(null)} />
            </Tabs.Content>
            <Tabs.Content value="repositories" className="h-full outline-none">
              <RepositoriesTab />
            </Tabs.Content>
            <Tabs.Content value="massrun" className="h-full outline-none">
              <MassRunTab />
            </Tabs.Content>
            <Tabs.Content value="account" className="h-full outline-none">
              <AccountTab />
            </Tabs.Content>
          </div>
        </Tabs.Root>

        <OperationsFooter />
      </div>
    </DashboardContext.Provider>
  );
}
