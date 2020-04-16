import { partialAsFull } from '@chainlink/ts-helpers'
import * as config from 'config'
import { Contract } from 'ethers'
import configureStore from 'redux-mock-store'
import thunk from 'redux-thunk'
import * as utils from '../../../contracts/utils'
import { Networks } from '../../../utils'
import * as operations from './operations'
import { INITIAL_STATE } from './reducers'

jest.mock('../../../contracts/utils')

const formatAnswerSpy = jest
  .spyOn(utils, 'formatAnswer')
  .mockImplementation(answer => answer)

const createContractSpy = jest
  .spyOn(utils, 'createContract')
  .mockImplementation(() => {
    const contract = partialAsFull<Contract>({
      latestAnswer: () => 'latestAnswer',
      currentAnswer: () => 'currentAnswer',
    })
    return contract
  })

jest.spyOn(config, 'getFeedsConfig').mockImplementation(() => {
  return [
    {
      contractAddress: '0xF79D6aFBb6dA890132F9D7c355e3015f15F3406F',
      contractType: 'aggregator',
      contractVersion: 2,
      name: 'ETH / USD',
      valuePrefix: '$',
      pair: ['ETH', 'USD'],
      heartbeat: 7200,
      path: 'eth-usd',
      networkId: 1,
      history: false,
      decimalPlaces: 3,
      multiply: '100000000',
      sponsored: ['Synthetix', 'Loopring', 'OpenLaw', '1inch'],
      threshold: 1,
      compareOffchain:
        'https://www.tradingview.com/symbols/ETHUSD/?exchange=COINBASE',
      healthPrice:
        'https://api.coingecko.com/api/v3/coins/markets?vs_currency=usd&ids=ethereum',
      listing: true,
    },
    {
      contractAddress: '0x79fEbF6B9F76853EDBcBc913e6aAE8232cFB9De9',
      contractType: 'aggregator',
      contractVersion: 1,
      name: 'ETH / USD',
      valuePrefix: '$',
      pair: ['ETH', 'USD'],
      heartbeat: 7200,
      path: 'eth-usd-depreciated',
      networkId: 1,
      history: true,
      decimalPlaces: 3,
      multiply: '100000000',
      sponsored: ['Synthetix', 'Loopring', 'OpenLaw', '1inch'],
      threshold: 1,
      compareOffchain:
        'https://www.tradingview.com/symbols/ETHUSD/?exchange=COINBASE',
      healthPrice:
        'https://api.coingecko.com/api/v3/coins/markets?vs_currency=usd&ids=ethereum',
      listing: true,
    },
  ]
})

const mainnetContracts = config
  .getFeedsConfig()
  .filter((config: any) => config.networkId === Networks.MAINNET)

const middlewares = [thunk]
const mockStore = configureStore(middlewares)
const store = mockStore(INITIAL_STATE)

const dispatchWrapper = (f: any) => (...args: any[]) => {
  return f(...args)(store.dispatch, store.getState)
}

describe('state/ducks/listing', () => {
  describe('fetchAnswers', () => {
    beforeEach(() => {
      store.clearActions()
      jest.clearAllMocks()
    })

    it('should fetch answer list', async () => {
      await dispatchWrapper(operations.fetchAnswers)()
      const actions = store.getActions()[0]
      expect(actions.type).toEqual('listing/SET_ANSWERS')
      expect(actions.payload).toHaveLength(mainnetContracts.length)

      const contractVersionOne = actions.payload.filter(
        (answer: any) => answer.config.contractVersion === 1,
      )[0]

      const contractVersionTwo = actions.payload.filter(
        (answer: any) => answer.config.contractVersion === 2,
      )[0]

      expect(contractVersionOne.answer).toEqual('currentAnswer')
      expect(contractVersionTwo.answer).toEqual('latestAnswer')
    })

    it('should build a list of objects', async () => {
      await dispatchWrapper(operations.fetchAnswers)()
      const actions = store.getActions()[0]
      expect(actions.payload[0]).toHaveProperty('answer')
      expect(actions.payload[0]).toHaveProperty('config')
    })

    it('should format answers', async () => {
      await dispatchWrapper(operations.fetchAnswers)()
      expect(formatAnswerSpy).toHaveBeenCalledTimes(mainnetContracts.length)
    })

    it('should create a contracts for each config', async () => {
      await dispatchWrapper(operations.fetchAnswers)()
      expect(createContractSpy).toHaveBeenCalledTimes(mainnetContracts.length)
    })
  })
})
