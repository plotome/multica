import { beforeEach, expect, it, vi } from "vitest";
import {
  render,
  screen,
  fireEvent,
  waitFor,
  cleanup,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ForkUpdateState } from "@multica/core/api";
import strings from "../../../../../../packages/views/locales/en/settings.json";

const mocks = vi.hoisted(() => ({ get: vi.fn(), act: vi.fn() }));
vi.mock("@multica/core/api", () => ({
  api: { getForkUpdateState: mocks.get, actOnForkUpdate: mocks.act },
}));
vi.mock("@multica/views/i18n", () => ({
  useT: () => ({
    t: (
      selector: (r: typeof strings) => unknown,
      values?: Record<string, unknown>,
    ) => {
      const value = selector(strings);
      return typeof value === "string"
        ? value.replace(/{{(\w+)}}/g, (_, key) => String(values?.[key] ?? ""))
        : value;
    },
  }),
}));
import { CoordinatedUpdatesPanel } from "./coordinated-updates-panel";

function fixture(): ForkUpdateState {
  const make = (id: string, host: string) => ({
    artifact: {
      id,
      host,
      kind: "cli-daemon",
      version: "2.0",
      commit: "b".repeat(40),
      component_versions: { cli: "2.0", daemon: "2.0" },
    },
    baseline: { commit: "a".repeat(40) },
    phase: "prepared",
    operation: "install",
    attempt: 1,
    rollback_available: false,
  });
  return {
    revision: 7,
    receipt: {
      digest: "c".repeat(64),
      approved: false,
      sequence: 2,
      targets: { mini: make("mini", "Mini"), air: make("air", "Air") },
    },
  };
}
function mount() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <CoordinatedUpdatesPanel />
    </QueryClientProvider>,
  );
}
beforeEach(() => {
  cleanup();
  mocks.get.mockReset();
  mocks.act.mockReset();
});

it("approves the displayed immutable candidate and revision once", async () => {
  const state = fixture();
  mocks.get.mockResolvedValue(state);
  mocks.act.mockResolvedValue({
    ...state,
    receipt: { ...state.receipt, approved: true },
  });
  mount();
  fireEvent.click(
    await screen.findByRole("button", { name: "Update all devices" }),
  );
  await waitFor(() =>
    expect(mocks.act).toHaveBeenCalledWith(state, "approve", undefined),
  );
  expect(mocks.act).toHaveBeenCalledTimes(1);
  expect(screen.getByText("2 update units")).toBeInTheDocument();
  expect(
    screen.queryByText("Every update unit has passed running verification."),
  ).not.toBeInTheDocument();
});

it("offers rollback for an attested successful device and names failed retry targets", async () => {
  const state = fixture();
  state.receipt.approved = true;
  state.receipt.targets.mini.phase = "succeeded";
  state.receipt.targets.mini.rollback_available = true;
  state.receipt.targets.air.phase = "failed";
  state.receipt.targets.air.error = "Package digest mismatch";
  state.receipt.targets.air.failure_stage = "validating";
  mocks.get.mockResolvedValue(state);
  mocks.act.mockResolvedValue(state);
  mount();
  expect(
    await screen.findByText("Package digest mismatch"),
  ).toBeInTheDocument();
  expect(
    screen.getByRole("button", { name: "Roll back · Mini · mini" }),
  ).toBeInTheDocument();
  expect(
    screen.queryByRole("button", { name: "Roll back · Air · air" }),
  ).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Retry · Air · air" }));
  await waitFor(() =>
    expect(mocks.act).toHaveBeenCalledWith(state, "retry", "air"),
  );
});

it("shows rollback failure separately and retains the actual reported running version", async () => {
  const state = fixture();
  state.receipt.approved = true;
  const target = state.receipt.targets.air;
  target.phase = "failed";
  target.operation = "rollback";
  target.failure_stage = "rollback-verification";
  target.error = "Connection unavailable";
  target.rollback_available = true;
  target.observation = {
    at: "2026-11-19T14:00:00Z",
    components: [{ kind: "cli", version: "1.0", commit: "a".repeat(40) }],
  };
  mocks.get.mockResolvedValue(state);
  mount();
  expect(
    await screen.findByText("Running: 1.0", { exact: false }),
  ).toBeInTheDocument();
  expect(
    screen.queryByRole("button", { name: "Roll back · Air · air" }),
  ).not.toBeInTheDocument();
  expect(
    screen.getByText("Failed stage: Verifying rollback"),
  ).toBeInTheDocument();
});

it("blocks approval for unknown states and malformed responses", async () => {
  const state = fixture();
  state.receipt.targets.air.phase = "future-phase";
  mocks.get.mockResolvedValue(state);
  mount();
  expect(
    await screen.findByRole("button", { name: "Update all devices" }),
  ).toBeDisabled();
  cleanup();
  mocks.get.mockResolvedValue(null);
  mount();
  expect(
    await screen.findByText("The update response could not be verified."),
  ).toBeInTheDocument();
  expect(
    screen.queryByRole("button", { name: "Update all devices" }),
  ).not.toBeInTheDocument();
});

it("shows no update without enabling installation", async () => {
  mocks.get.mockResolvedValue({ available: false });
  mount();
  expect(await screen.findByText("No updates available.")).toBeInTheDocument();
  expect(
    screen.queryByRole("button", { name: "Update all devices" }),
  ).not.toBeInTheDocument();
});

it("keeps candidate failure separate from baseline restoration and actual version", async () => {
  const state = fixture();
  state.receipt.approved = true;
  const target = state.receipt.targets.mini;
  target.phase = "failed";
  target.failure_stage = "running-verification";
  target.error = "candidate unhealthy";
  target.recovery = {
    status: "restored",
    candidate_error: "candidate unhealthy",
  };
  target.observation = {
    at: "2026-11-20T00:00:00Z",
    components: [
      { kind: "cli", version: "1.0", commit: "a".repeat(40) },
      { kind: "daemon", version: "1.0", commit: "a".repeat(40) },
    ],
  };
  mocks.get.mockResolvedValue(state);
  mount();
  expect(
    (await screen.findAllByText(/candidate unhealthy/)).length,
  ).toBeGreaterThan(0);
  expect(
    screen.queryByText("Every update unit has passed running verification."),
  ).not.toBeInTheDocument();
  expect(
    screen.getByRole("button", { name: /Retry candidate update/ }),
  ).toBeInTheDocument();
  expect(screen.getAllByText(/1.0/).length).toBeGreaterThan(0);
});
