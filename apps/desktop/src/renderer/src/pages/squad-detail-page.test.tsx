// @vitest-environment jsdom
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
import { SquadDetailPage } from "./squad-detail-page";

vi.mock("@multica/views/squads", () => ({
  SquadDetailPage: ({ squadId }: { squadId: string }) => <div data-testid="squad-id">{squadId}</div>,
}));

describe("desktop squad detail route", () => {
  it("passes its own route ID to the shared view", () => {
    const id = "4f15ace0-c1c2-4117-a3a5-5160b992702d";
    render(
      <MemoryRouter initialEntries={[`/workspace/squads/${id}`]}>
        <Routes>
          <Route path="/:workspaceSlug/squads/:id" element={<SquadDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );
    expect(screen.getByTestId("squad-id").textContent).toBe(id);
  });
});
