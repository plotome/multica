// @vitest-environment node
import { describe, expect, it } from "vitest";
import { taskElicitationListSchema, type TaskElicitation } from "./elicitation-schema";
import { parseWithFallback } from "../api/schema";
const approval = { id: "017fa8d2-51b5-4337-a440-1d024349c105", status: "pending", request: { threadId: "thread", turnId: "turn", serverName: "cua_repl", message: "Allow?", _meta: { tool_params: { app: "fixture" } } } };
describe("approval API compatibility", () => {
  it("defaults unoffered persistence to no persistent choices", () => {
    expect(taskElicitationListSchema.parse([approval])[0]?.request._meta.persist).toEqual([]);
  });
  it.each([null, {}, [{ ...approval, id: "bad" }], [{ ...approval, request: {} }], [{ ...approval, status: "future" }]])("fails closed on malformed or unknown approvals", (raw) => {
    expect(parseWithFallback<TaskElicitation[]>(raw, taskElicitationListSchema, [], { endpoint: "test" })).toEqual([]);
  });
});
