// @vitest-environment node
import { expect, it } from "vitest";
import { ForkUpdateStateSchema } from "./fork-update-schema";

it("rejects malformed receipts rather than enabling actions", () => {
  for (const value of [
    null,
    {},
    {
      revision: 1,
      receipt: { digest: "bad", approved: true, sequence: 1, targets: {} },
    },
  ])
    expect(ForkUpdateStateSchema.safeParse(value).success).toBe(false);
});
