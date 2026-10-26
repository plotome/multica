import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../../test/i18n";
import { TaskApprovalPanel } from "./task-approval-panel";
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace" }));
vi.mock("@multica/core/api", async (original) => ({ ...await original<typeof import("@multica/core/api")>(), api: { listTaskElicitations: vi.fn(), respondTaskElicitation: vi.fn() } }));
const request = { threadId: "thread", turnId: "turn", serverName: "cua_repl", message: "Allow Fixture?", _meta: { tool_params: { app: "fixture.app" }, persist: ["session"] } };
function renderPanel(active = true) {
  return renderWithI18n(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><TaskApprovalPanel taskId="task" active={active} /></QueryClientProvider>);
}
beforeEach(() => { vi.mocked(api.listTaskElicitations).mockResolvedValue([{ id: "017fa8d2-51b5-4337-a440-1d024349c105", status: "pending", request }]); vi.mocked(api.respondTaskElicitation).mockResolvedValue(); });
describe("application approval panel", () => {
  it("shows the original request and sends only the explicit human choice", async () => {
    renderPanel();
    expect(await screen.findByText("Allow Fixture?")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Always allow" })).toBeNull();
    expect(api.respondTaskElicitation).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Decline" }));
    await waitFor(() => expect(api.respondTaskElicitation).toHaveBeenCalledWith("task", "017fa8d2-51b5-4337-a440-1d024349c105", "decline"));
  });
  it("does not fetch while the transcript is closed", () => { vi.mocked(api.listTaskElicitations).mockClear(); renderPanel(false); expect(api.listTaskElicitations).not.toHaveBeenCalled(); });
  it("shows an exact browser origin and offers a one-time grant", async () => {
    vi.mocked(api.listTaskElicitations).mockResolvedValue([{ id: "017fa8d2-51b5-4337-a440-1d024349c105", status: "pending", request: { ...request, _meta: { origin: "https://multica.ploto.cc", persist: [] } } }]);
    renderPanel();
    expect(await screen.findByText("cua_repl · https://multica.ploto.cc")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Allow for this session" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Always allow" })).toBeNull();
  });
  it("leaves a failed response visible with retry feedback", async () => {
    vi.mocked(api.respondTaskElicitation).mockRejectedValue(new Error("offline")); renderPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Allow once" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("could not be saved");
  });
});
