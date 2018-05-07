import produce from "immer"
import { read, BASE_URL } from "shared/modules/config"
import { open, COLOR_DANGER } from "shared/modules/alert"
import { getToken } from "shared/modules/authentication"

export const LOADING = "clinics/LOADING"
export const LOADED = "clinics/LOADED"
export const FAILED = "clinics/FAILED"

const initialState = {
    cache: [],
    loading: false,
    loaded: true,
    failed: true
}

export default (state = initialState, action) => {
    return produce(state, draft => {
        switch (action.type) {
            case LOADING:
                draft.loading = true
                draft.loaded = draft.failed = false
                break

            case LOADED:
                // add clinic to cache
                draft.cache[action.id] = action.data

                // mark as loaded
                draft.loading = false
                draft.loaded = true
                break

            case FAILED:
                draft.loading = draft.loaded = false
                draft.failed = true
                break

            default:
        }
    })
}

export const load = id => (dispatch, getState) => {
    // check cache
    const cache = getState().clinics.cache
    if (cache[id]) {
        return Promise.resolve(cache[id])
    }

    dispatch({ type: LOADING })
    const url = `${read(BASE_URL)}/auth/clinics/${id}`

    return fetch(url, {
        method: "GET",
        headers: {
            Authorization: dispatch(getToken()),
            "Content-Type": "application/json"
        }
    })
        .then(response => Promise.all([response.ok, response.json()]))
        .then(([ok, data]) => {
            if (!ok) {
                throw new Error("Failed to load clinic data")
            }
            dispatch({ type: LOADED, id, data })
            return data
        })
        .catch(ex => {
            dispatch(open("Failed to fetch clinic :: " + ex.message, COLOR_DANGER))
            dispatch({ type: FAILED, id })
        })
}
