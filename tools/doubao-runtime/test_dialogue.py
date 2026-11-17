import unittest
import bridge

class DialogueBoundaryTests(unittest.TestCase):
    def test_only_private_validated_conversation_cursor_is_accepted(self):
        request={'operation':'prepare_text','port':19227,'identity':'a'*64,'model':'doubao:synthetic:model','prompt':'synthetic turn'}
        bridge.validate(request)
        bridge.validate({**request,'conversation':'synthetic-conversation'})
        for value in ['',None,123,[],{},'x'*129,'foreign\nconversation','https://invalid.example']:
            with self.subTest(kind=type(value).__name__),self.assertRaises(bridge.BridgeError):
                bridge.validate({**request,'conversation':value})
        with self.assertRaises(bridge.BridgeError):bridge.validate({**request,'conversation':'synthetic','tools':[{}]})

    def test_reward_read_accepts_no_mutation_arguments(self):
        request={'operation':'reward_records','port':19227,'identity':'a'*64}
        bridge.validate(request)
        for field in ['task','group','instance','url','script']:
            with self.subTest(field=field),self.assertRaises(bridge.BridgeError):bridge.validate({**request,field:'synthetic'})

    def test_work_read_has_exact_scope_and_no_action(self):
        request={'operation':'work_read','port':19227,'identity':'a'*64,'conversation':'synthetic-conversation'}
        bridge.validate(request)
        bridge.validate({**request,'session':'synthetic-session','message':'synthetic-message'})
        for extra in [{'session':'only-session'},{'url':'https://invalid.example'},{'prompt':'do it again'},{'message':'missing-session'},{'conversation':'../foreign'}]:
            with self.assertRaises(bridge.BridgeError):bridge.validate({**request,**extra})

if __name__=='__main__':unittest.main()
