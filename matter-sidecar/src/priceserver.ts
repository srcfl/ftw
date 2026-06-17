// Phase 2 — 42W as a Matter *server*: exposes spot-price + forecast data via
// the Matter 1.5 CommodityPrice cluster (on an ElectricalEnergyTariffDevice
// endpoint) so other Matter controllers can read it without touching 42W's
// own REST API. The Go side owns all unit/timestamp conversion (öre/kWh →
// CommodityPrice's currency-minor-units, Unix ms → Matter epoch-s); this
// module just holds the endpoint and patches its state.
import { Endpoint } from "@matter/node";
import { ElectricalEnergyTariffDevice } from "@matter/node/devices";
import { CommodityPriceServer } from "@matter/node/behaviors";
import { TariffUnit } from "@matter/types/globals";

const PriceDeviceType = ElectricalEnergyTariffDevice.with(CommodityPriceServer.with("Forecasting"));

export type PriceEndpoint = Endpoint<typeof PriceDeviceType>;

export interface PricePeriod {
  periodStart: number;
  periodEnd: number | null;
  price: number;
}

export function createPriceEndpoint(): PriceEndpoint {
  return new Endpoint(PriceDeviceType, {
    id: "price",
    commodityPrice: {
      tariffUnit: TariffUnit.KWh,
      currency: { currency: 752, decimalPoints: 2 }, // SEK, minor unit = öre
      currentPrice: null,
      priceForecast: [],
    },
  });
}

export async function setPriceFeed(
  endpoint: PriceEndpoint,
  current: PricePeriod | null,
  forecast: PricePeriod[],
): Promise<void> {
  await endpoint.set({
    commodityPrice: {
      currentPrice: current,
      priceForecast: forecast,
    },
  });
}
