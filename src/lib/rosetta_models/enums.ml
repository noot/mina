type curvetype = (* pallas *) string [@@deriving yojson, show]

type signaturetype = (* schnorr_poseidon *) string [@@deriving yojson, show]

type coinaction = () [@@deriving yojson, show, eq]
