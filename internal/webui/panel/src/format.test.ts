import { afterEach, describe, expect, it } from "vitest";
import { bytes, day, duration, millis, utcDay } from "./format";
import { setCatalogue } from "./i18n";

afterEach(() => setCatalogue("en", {}));

describe("format", () => {
  it("writes durations and milliseconds", () => {
    expect(duration(59)).toBe("59s");
    expect(duration(61)).toBe("1m 1s");
    expect(duration(7200)).toBe("2h");
    expect(duration(null)).toBe("—");
    expect(millis(450)).toBe("450ms");
    expect(millis(1500)).toBe("2s");
    expect(millis(undefined)).toBe("—");
  });

  it("writes bytes in binary units", () => {
    expect(bytes(512)).toBe("512 B");
    expect(bytes(1536)).toBe("1.5 KiB");
    expect(bytes(4 * 1024 ** 3)).toBe("4 GiB");
    expect(bytes(Number.NaN)).toBe("—");
  });

  it("reads UTC day keys", () => {
    const now = Date.parse("2026-11-28T23:30:00Z");
    expect(utcDay(0, now)).toBe("2026-11-28");
    expect(utcDay(-29, now)).toBe("2026-10-30");
    expect(day("2026-11-01", true)).toBe("Nov 1, 2026");
    expect(day("nope")).toBe("nope");
  });
});
